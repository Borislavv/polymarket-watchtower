// ws_selector_debug.go — v11.11 read-only WS subscription explainer.
//
// Answers the operator question "which tokens does Watchtower actually
// subscribe to, and why?" by running the live hot-mode selector query
// against Postgres and annotating each emitted (market, token) row
// with:
//
//   - winning priority bucket (1 blocked/active_catalyst, 2 watching,
//     3 recent_alert, 4 active_catalyst_event, 5 fresh_repricing,
//     6 recent_annotation);
//   - market_slug + outcome label;
//   - event_slug;
//   - the per-bucket source (prediction state / alert dedup_key /
//     catalyst title / repricing status / annotation timestamp).
//
// Read-only. No Telegram, no AI, no writes.
//
// Run:
//
//	go run ./cmd/cli ws-selector-debug --dsn=$POSTGRES_DSN \
//	    --limit 5000 [--mode hot|predictions|alerts|all_active_limited] [--json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runWSSelectorDebug(args []string) {
	fs := flag.NewFlagSet("ws-selector-debug", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	limit := fs.Int("limit", 100, "max markets to emit (mirrors WS_MAX_MARKETS)")
	mode := fs.String("mode", "hot", "selector mode (hot|predictions|alerts|all_active_limited)")
	jsonOut := fs.Bool("json", false, "JSON instead of operator-facing table")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "ws-selector-debug: --dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws-selector-debug: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	rep, err := gatherWSSelectorReport(ctx, pool, *mode, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws-selector-debug: gather: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	printWSSelectorReport(os.Stdout, rep)
}

// WSSelectorRow is one (market, token) row from the selector, annotated.
type WSSelectorRow struct {
	Rank        int    `json:"rank"`
	Priority    int    `json:"priority"`
	BucketName  string `json:"bucket_name"`
	EventSlug   string `json:"event_slug"`
	ConditionID string `json:"condition_id"`
	MarketSlug  string `json:"market_slug"`
	TokenID     string `json:"clob_token_id"`
	Outcome     string `json:"outcome"`
}

type WSSelectorReport struct {
	GeneratedAt        time.Time       `json:"generated_at"`
	Mode               string          `json:"mode"`
	Limit              int             `json:"limit"`
	TotalRows          int             `json:"total_rows"`
	UniqueMarkets      int             `json:"unique_markets"`
	UniqueEvents       int             `json:"unique_events"`
	UniqueTokens       int             `json:"unique_tokens"`
	MaxTokensPerMarket int             `json:"max_tokens_per_market"`
	MaxMarketsPerEvent int             `json:"max_markets_per_event"`
	TopEventShare      float64         `json:"top_event_share_pct"`
	PerBucket          map[string]int  `json:"per_bucket"`
	PerEvent           []WSEventCount  `json:"per_event"`
	Rows               []WSSelectorRow `json:"rows"`
}

type WSEventCount struct {
	EventSlug string `json:"event_slug"`
	Markets   int    `json:"markets"`
	Tokens    int    `json:"tokens"`
}

func bucketName(p int) string {
	switch p {
	case 1:
		return "blocked_or_active_catalyst_prediction"
	case 2:
		return "watching_prediction"
	case 3:
		return "recent_alert"
	case 4:
		return "active_catalyst_event"
	case 5:
		return "fresh_repricing"
	case 6:
		return "recent_annotation"
	}
	return "unknown"
}

func gatherWSSelectorReport(ctx context.Context, pool *pgxpool.Pool, mode string, limit int) (*WSSelectorReport, error) {
	rep := &WSSelectorReport{
		GeneratedAt: time.Now().UTC(),
		Mode:        mode,
		Limit:       limit,
		PerBucket:   map[string]int{},
	}

	// We replicate the production selector SQL here ANNOTATED with
	// priority. The only delta vs production: we expose the priority
	// column to the operator.
	sql := `
WITH hot_set AS (
    SELECT event_slug, condition_id, 1 AS priority
    FROM polymarket_market_predictions
    WHERE current_state IN ('blocked','active_catalyst') AND archived_at IS NULL
    UNION ALL
    SELECT event_slug, condition_id, 2
    FROM polymarket_market_predictions
    WHERE current_state NOT IN ('resolved','invalidated','stale','blocked','active_catalyst')
      AND archived_at IS NULL
    UNION ALL
    SELECT pm.event_slug, pm.condition_id, 3
    FROM polymarket_alerts a JOIN polymarket_markets pm ON pm.id = a.market_id
    WHERE a.created_at > NOW() - INTERVAL '24 hours'
    UNION ALL
    SELECT pm.event_slug, pm.condition_id, 4
    FROM polymarket_event_catalysts c
    JOIN polymarket_markets pm ON pm.event_slug = c.event_slug
    WHERE c.status IN ('active','expected')
    UNION ALL
    SELECT event_slug, condition_id, 5
    FROM polymarket_repricing_signals
    WHERE created_at > NOW() - INTERVAL '24 hours'
      AND (repricing_status IN ('underreacting','overreacting','still_repricing','reversed')
           OR flow_timing IN ('pre_event_positioning','post_event_chasing'))
    UNION ALL
    SELECT DISTINCT pm.event_slug, pm.condition_id, 6
    FROM polymarket_event_annotations an
    JOIN polymarket_markets pm ON pm.event_slug = an.event_slug
    WHERE an.timestamp > NOW() - INTERVAL '12 hours'
),
ranked AS (
    SELECT event_slug, condition_id, MIN(priority) AS priority
    FROM hot_set GROUP BY event_slug, condition_id
    ORDER BY priority ASC LIMIT $1
)
SELECT r.priority::int AS priority,
       m.event_slug, m.condition_id, m.market_slug,
       tok.token AS clob_token_id,
       COALESCE(out.outcome, '') AS outcome
FROM (
    SELECT DISTINCT ON (em.condition_id)
           em.event_slug, em.condition_id, em.market_slug,
           em.clob_token_ids_json, em.outcomes_json, em.created_at
    FROM polymarket_event_page_markets em
    WHERE em.created_at > NOW() - INTERVAL '24 hours' AND em.active = TRUE
    ORDER BY em.condition_id, em.created_at DESC
) m
JOIN ranked r ON r.event_slug = m.event_slug AND r.condition_id = m.condition_id
CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(m.clob_token_ids_json, '[]'::jsonb))
    WITH ORDINALITY AS tok(token, ord)
LEFT JOIN LATERAL (
    SELECT o.outcome FROM jsonb_array_elements_text(COALESCE(m.outcomes_json, '[]'::jsonb))
        WITH ORDINALITY AS o(outcome, ord) WHERE o.ord = tok.ord LIMIT 1
) AS out ON TRUE
ORDER BY r.priority ASC, m.event_slug ASC, m.condition_id ASC, tok.ord ASC
LIMIT $1`

	rows, err := pool.Query(ctx, sql, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("selector query: %w", err)
	}
	defer rows.Close()

	uniqMarkets := map[string]struct{}{}
	uniqEvents := map[string]struct{}{}
	uniqTokens := map[string]struct{}{}
	tokensPerMarket := map[string]int{}
	marketsPerEvent := map[string]map[string]struct{}{}
	tokensPerEvent := map[string]map[string]struct{}{}

	rank := 0
	for rows.Next() {
		var priority int
		var row WSSelectorRow
		if err := rows.Scan(&priority, &row.EventSlug, &row.ConditionID,
			&row.MarketSlug, &row.TokenID, &row.Outcome); err != nil {
			return nil, err
		}
		rank++
		row.Rank = rank
		row.Priority = priority
		row.BucketName = bucketName(priority)
		rep.Rows = append(rep.Rows, row)
		rep.PerBucket[row.BucketName]++
		uniqMarkets[row.ConditionID] = struct{}{}
		uniqEvents[row.EventSlug] = struct{}{}
		uniqTokens[row.TokenID] = struct{}{}
		tokensPerMarket[row.ConditionID]++
		if marketsPerEvent[row.EventSlug] == nil {
			marketsPerEvent[row.EventSlug] = map[string]struct{}{}
		}
		marketsPerEvent[row.EventSlug][row.ConditionID] = struct{}{}
		if tokensPerEvent[row.EventSlug] == nil {
			tokensPerEvent[row.EventSlug] = map[string]struct{}{}
		}
		tokensPerEvent[row.EventSlug][row.TokenID] = struct{}{}
	}
	rep.TotalRows = rank
	rep.UniqueMarkets = len(uniqMarkets)
	rep.UniqueEvents = len(uniqEvents)
	rep.UniqueTokens = len(uniqTokens)
	for _, c := range tokensPerMarket {
		if c > rep.MaxTokensPerMarket {
			rep.MaxTokensPerMarket = c
		}
	}
	for ev, set := range marketsPerEvent {
		c := len(set)
		if c > rep.MaxMarketsPerEvent {
			rep.MaxMarketsPerEvent = c
		}
		rep.PerEvent = append(rep.PerEvent, WSEventCount{
			EventSlug: ev,
			Markets:   c,
			Tokens:    len(tokensPerEvent[ev]),
		})
	}
	sort.Slice(rep.PerEvent, func(i, j int) bool {
		if rep.PerEvent[i].Tokens != rep.PerEvent[j].Tokens {
			return rep.PerEvent[i].Tokens > rep.PerEvent[j].Tokens
		}
		return rep.PerEvent[i].EventSlug < rep.PerEvent[j].EventSlug
	})
	if rep.TotalRows > 0 && len(rep.PerEvent) > 0 {
		rep.TopEventShare = 100.0 * float64(rep.PerEvent[0].Tokens) / float64(rep.TotalRows)
	}
	return rep, nil
}

func printWSSelectorReport(out *os.File, r *WSSelectorReport) {
	fmt.Fprintf(out, "WS selector — mode=%s, limit=%d, generated %s\n\n",
		r.Mode, r.Limit, r.GeneratedAt.Format(time.RFC3339))

	fmt.Fprintf(out, "Summary:\n")
	fmt.Fprintf(out, "  rows emitted:           %d\n", r.TotalRows)
	fmt.Fprintf(out, "  unique markets:         %d\n", r.UniqueMarkets)
	fmt.Fprintf(out, "  unique events:          %d\n", r.UniqueEvents)
	fmt.Fprintf(out, "  unique tokens:          %d\n", r.UniqueTokens)
	fmt.Fprintf(out, "  max tokens / market:    %d\n", r.MaxTokensPerMarket)
	fmt.Fprintf(out, "  max markets / event:    %d\n", r.MaxMarketsPerEvent)
	fmt.Fprintf(out, "  top-event share:        %.1f%%\n", r.TopEventShare)

	if len(r.PerBucket) > 0 {
		fmt.Fprintln(out, "\nPer-bucket (winning priority per market):")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  bucket\trows")
		keys := make([]string, 0, len(r.PerBucket))
		for k := range r.PerBucket {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(tw, "  %s\t%d\n", k, r.PerBucket[k])
		}
		_ = tw.Flush()
	}

	if len(r.PerEvent) > 0 {
		topN := 25
		if topN > len(r.PerEvent) {
			topN = len(r.PerEvent)
		}
		fmt.Fprintf(out, "\nTop %d events by token count:\n", topN)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  event_slug\tmarkets\ttokens")
		for _, e := range r.PerEvent[:topN] {
			fmt.Fprintf(tw, "  %s\t%d\t%d\n", e.EventSlug, e.Markets, e.Tokens)
		}
		_ = tw.Flush()
	}

	if len(r.Rows) > 0 {
		// Cap printed rows at 50 for readability; full set is always
		// available via --json.
		maxPrint := 50
		if maxPrint > len(r.Rows) {
			maxPrint = len(r.Rows)
		}
		fmt.Fprintf(out, "\nFirst %d (token, outcome, market, event, bucket):\n", maxPrint)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  rank\tbucket\tevent_slug\tmarket_slug\toutcome\ttoken_id")
		for _, row := range r.Rows[:maxPrint] {
			tok := row.TokenID
			if len(tok) > 16 {
				tok = tok[:16] + "…"
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\n",
				row.Rank, row.BucketName, row.EventSlug, row.MarketSlug, row.Outcome, tok)
		}
		_ = tw.Flush()
		if len(r.Rows) > maxPrint {
			fmt.Fprintf(out, "  … (%d more rows — use --json for the full set)\n", len(r.Rows)-maxPrint)
		}
	}

	fmt.Fprintln(out, "\nUser flow / AI / writes: none (read-only).")
}
