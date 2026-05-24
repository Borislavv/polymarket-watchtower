// ws_selector_debug.go — v11.12-insider-prior read-only WS selector
// explainer.
//
// Answers the operator question "which markets does Watchtower
// subscribe to via WS, and ALL their outcome tokens?" by replaying
// the live hot-mode selector query and showing every (market, token)
// row with its priority bucket.
//
// v11.12-insider-prior bucket scheme (no prediction state):
//
//	1 — operator_pinned
//	2 — recent_alert (24h)
//	3 — active_or_expected_catalyst
//	4 — repricing_signal (24h)
//	5 — event_annotation_recent (default 168h = 7d)
//	6 — high_trade_market (opt-in)
//
// Read-only. No Telegram, no AI, no writes.
//
// Run:
//
//	go run ./cmd/cli ws-selector-debug --dsn=$POSTGRES_DSN \
//	    --limit-markets=$WS_MAX_MARKETS [--show-tokens] [--json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runWSSelectorDebug(args []string) {
	fs := flag.NewFlagSet("ws-selector-debug", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	// Both --limit (legacy alias) and --limit-markets are accepted;
	// the spec's --limit-markets name is canonical.
	limit := fs.Int("limit", 0, "alias for --limit-markets (legacy)")
	limitMarkets := fs.Int("limit-markets", 100, "max MARKETS to emit (mirrors WS_MAX_MARKETS)")
	mode := fs.String("mode", "hot", "selector mode (hot|alerts) — prediction modes removed in v11.12-insider-prior")
	annotationLookback := fs.Duration("annotation-lookback", 168*time.Hour,
		"freshness window for bucket 5 — default 168h = 7d to match linkup cadence")
	includeHighTrade := fs.Bool("include-high-trade", false, "enable bucket 6 (high_trade_market)")
	highTradeMin := fs.Int("high-trade-min-trades", 50, "min trades for bucket 6 over lookback")
	highTradeLookback := fs.Int("high-trade-lookback-hours", 24, "lookback hours for bucket 6")
	pinnedCSV := fs.String("operator-pinned", os.Getenv("WORKER_OPERATOR_PINNED_CONDITION_IDS"),
		"comma-separated operator-pinned condition_ids (priority 1)")
	showTokens := fs.Bool("show-tokens", false, "print per-market token expansion (every clob_token_id)")
	jsonOut := fs.Bool("json", false, "JSON instead of operator-facing table")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "ws-selector-debug: --dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}
	maxMarkets := *limitMarkets
	if *limit > 0 {
		maxMarkets = *limit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws-selector-debug: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	rep, err := gatherWSSelectorReport(ctx, pool, *mode, maxMarkets, gatherOptions{
		AnnotationLookback:       *annotationLookback,
		IncludeHighTrade:         *includeHighTrade,
		HighTradeMinTrades:       *highTradeMin,
		HighTradeLookbackHours:   *highTradeLookback,
		OperatorPinnedConditions: splitCSV(*pinnedCSV),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws-selector-debug: gather: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	printWSSelectorReport(os.Stdout, rep, *showTokens)
}

// WSSelectorRow is one (market, token) row from the selector, annotated.
type WSSelectorRow struct {
	Rank        int    `json:"rank"`
	RankMarket  int    `json:"rank_market"`
	Priority    int    `json:"priority"`
	BucketName  string `json:"bucket_name"`
	EventSlug   string `json:"event_slug"`
	ConditionID string `json:"condition_id"`
	MarketSlug  string `json:"market_slug"`
	TokenID     string `json:"clob_token_id"`
	Outcome     string `json:"outcome"`
	TokenIndex  int    `json:"token_index"`
}

// WSMarketSummary captures one market's full outcome-token expansion.
type WSMarketSummary struct {
	RankMarket  int             `json:"rank_market"`
	Priority    int             `json:"priority"`
	BucketName  string          `json:"bucket_name"`
	EventSlug   string          `json:"event_slug"`
	ConditionID string          `json:"condition_id"`
	MarketSlug  string          `json:"market_slug"`
	TokenCount  int             `json:"token_count"`
	Tokens      []WSTokenDetail `json:"tokens"`
}

type WSTokenDetail struct {
	Index   int    `json:"token_index"`
	TokenID string `json:"clob_token_id"`
	Outcome string `json:"outcome"`
}

type WSEventCount struct {
	EventSlug string `json:"event_slug"`
	Markets   int    `json:"markets"`
	Tokens    int    `json:"tokens"`
}

type WSSelectorReport struct {
	GeneratedAt              time.Time         `json:"generated_at"`
	Mode                     string            `json:"mode"`
	LimitMarkets             int               `json:"limit_markets"`
	AnnotationLookback       string            `json:"annotation_lookback"`
	BucketsUsed              []string          `json:"buckets_used"`
	PredictionBucketsPresent bool              `json:"prediction_buckets_present"`
	TotalRows                int               `json:"total_rows"`
	UniqueMarkets            int               `json:"unique_markets"`
	UniqueEvents             int               `json:"unique_events"`
	UniqueTokens             int               `json:"unique_tokens"`
	MaxTokensPerMarket       int               `json:"max_tokens_per_market"`
	MarketsWithMoreThan2Tk   int               `json:"markets_with_more_than_2_tokens"`
	MaxMarketsPerEvent       int               `json:"max_markets_per_event"`
	TotalTokenExpansion      int               `json:"total_token_expansion"`
	DroppedTokens            int               `json:"dropped_tokens"`
	PerBucket                map[string]int    `json:"per_bucket"`
	PerEvent                 []WSEventCount    `json:"per_event"`
	Markets                  []WSMarketSummary `json:"markets"`
	Rows                     []WSSelectorRow   `json:"rows"`
}

type gatherOptions struct {
	AnnotationLookback       time.Duration
	IncludeHighTrade         bool
	HighTradeMinTrades       int
	HighTradeLookbackHours   int
	OperatorPinnedConditions []string
}

// bucketName is the v11.12-insider-prior naming. Prediction labels
// (blocked_or_active_catalyst_prediction / watching_prediction) are
// GONE — if the SQL ever re-introduces them, the priority order
// changes and the test pin in selector_test.go fires first.
func bucketName(p int) string {
	switch p {
	case 1:
		return "operator_pinned"
	case 2:
		return "recent_alert"
	case 3:
		return "active_or_expected_catalyst"
	case 4:
		return "repricing_signal"
	case 5:
		return "event_annotation_recent"
	case 6:
		return "high_trade_market"
	}
	return "unknown"
}

func gatherWSSelectorReport(ctx context.Context, pool *pgxpool.Pool, mode string, limit int, opt gatherOptions) (*WSSelectorReport, error) {
	rep := &WSSelectorReport{
		GeneratedAt:        time.Now().UTC(),
		Mode:               mode,
		LimitMarkets:       limit,
		AnnotationLookback: opt.AnnotationLookback.String(),
		PerBucket:          map[string]int{},
		BucketsUsed: []string{
			"operator_pinned",
			"recent_alert",
			"active_or_expected_catalyst",
			"repricing_signal",
			"event_annotation_recent",
		},
	}
	if opt.IncludeHighTrade {
		rep.BucketsUsed = append(rep.BucketsUsed, "high_trade_market")
	}
	hours := int(opt.AnnotationLookback.Hours())
	if hours <= 0 {
		hours = 168
	}
	if opt.HighTradeMinTrades <= 0 {
		opt.HighTradeMinTrades = 50
	}
	if opt.HighTradeLookbackHours <= 0 {
		opt.HighTradeLookbackHours = 24
	}

	highTradeCTE := ""
	if opt.IncludeHighTrade {
		highTradeCTE = fmt.Sprintf(`
    UNION ALL
    SELECT pm.event_slug, pm.condition_id, 6 AS priority
    FROM polymarket_markets pm
    JOIN polymarket_trades t ON t.market_id = pm.id
    WHERE pm.active = TRUE AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
      AND t.traded_at >= NOW() - INTERVAL '%d hours'
    GROUP BY pm.event_slug, pm.condition_id
    HAVING COUNT(t.id) >= %d`, opt.HighTradeLookbackHours, opt.HighTradeMinTrades)
	}

	sql := fmt.Sprintf(`
WITH hot_set AS (
    -- bucket 1 operator_pinned
    SELECT pm.event_slug, pm.condition_id, 1 AS priority
    FROM polymarket_markets pm
    WHERE pm.condition_id = ANY($2::text[])
    UNION ALL
    -- bucket 2 recent_alert
    SELECT pm.event_slug, pm.condition_id, 2
    FROM polymarket_alerts a JOIN polymarket_markets pm ON pm.id = a.market_id
    WHERE a.created_at > NOW() - INTERVAL '24 hours'
    UNION ALL
    -- bucket 3 active_or_expected_catalyst
    SELECT pm.event_slug, pm.condition_id, 3
    FROM polymarket_event_catalysts c
    JOIN polymarket_markets pm ON pm.event_slug = c.event_slug
    WHERE c.status IN ('active','expected')
    UNION ALL
    -- bucket 4 repricing_signal
    SELECT event_slug, condition_id, 4
    FROM polymarket_repricing_signals
    WHERE created_at > NOW() - INTERVAL '24 hours'
      AND (repricing_status IN ('underreacting','overreacting','still_repricing','reversed')
           OR flow_timing IN ('pre_event_positioning','post_event_chasing'))
    UNION ALL
    -- bucket 5 event_annotation_recent
    SELECT DISTINCT pm.event_slug, pm.condition_id, 5
    FROM polymarket_event_annotations an
    JOIN polymarket_markets pm ON pm.event_slug = an.event_slug
    WHERE an.timestamp > NOW() - INTERVAL '%d hours'
%s
),
ranked AS (
    SELECT event_slug, condition_id, MIN(priority) AS priority
    FROM hot_set GROUP BY event_slug, condition_id
    ORDER BY priority ASC, condition_id ASC
    LIMIT $1
)
SELECT r.priority::int AS priority,
       m.event_slug, m.condition_id, m.market_slug,
       tok.token AS clob_token_id,
       COALESCE(out.outcome, '') AS outcome,
       tok.ord::int AS token_index
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
ORDER BY r.priority ASC, m.condition_id ASC, tok.ord ASC
`, hours, highTradeCTE)

	pins := opt.OperatorPinnedConditions
	if pins == nil {
		pins = []string{}
	}
	rows, err := pool.Query(ctx, sql, int32(limit), pins)
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
	marketByCondition := map[string]*WSMarketSummary{}
	marketOrder := []string{}

	rank := 0
	for rows.Next() {
		var priority int
		var row WSSelectorRow
		if err := rows.Scan(&priority, &row.EventSlug, &row.ConditionID,
			&row.MarketSlug, &row.TokenID, &row.Outcome, &row.TokenIndex); err != nil {
			return nil, err
		}
		rank++
		row.Rank = rank
		row.Priority = priority
		row.BucketName = bucketName(priority)
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

		// per-market expansion
		sum, ok := marketByCondition[row.ConditionID]
		if !ok {
			sum = &WSMarketSummary{
				Priority:    priority,
				BucketName:  row.BucketName,
				EventSlug:   row.EventSlug,
				ConditionID: row.ConditionID,
				MarketSlug:  row.MarketSlug,
			}
			marketByCondition[row.ConditionID] = sum
			marketOrder = append(marketOrder, row.ConditionID)
		}
		sum.Tokens = append(sum.Tokens, WSTokenDetail{
			Index: row.TokenIndex, TokenID: row.TokenID, Outcome: row.Outcome,
		})
		sum.TokenCount = len(sum.Tokens)
		row.RankMarket = len(marketOrder)
		rep.Rows = append(rep.Rows, row)
	}
	// finalise per-market summaries with stable rank
	for idx, cid := range marketOrder {
		sum := marketByCondition[cid]
		sum.RankMarket = idx + 1
		rep.Markets = append(rep.Markets, *sum)
	}
	rep.TotalRows = rank
	rep.UniqueMarkets = len(uniqMarkets)
	rep.UniqueEvents = len(uniqEvents)
	rep.UniqueTokens = len(uniqTokens)
	rep.TotalTokenExpansion = rank
	// DroppedTokens: by construction the new selector cannot drop a
	// token. The field exists so the JSON output explicitly reports
	// zero — operators expect the value, and a future regression
	// would have to set it manually.
	rep.DroppedTokens = 0
	for _, c := range tokensPerMarket {
		if c > rep.MaxTokensPerMarket {
			rep.MaxTokensPerMarket = c
		}
		if c > 2 {
			rep.MarketsWithMoreThan2Tk++
		}
	}
	for ev, set := range marketsPerEvent {
		c := len(set)
		if c > rep.MaxMarketsPerEvent {
			rep.MaxMarketsPerEvent = c
		}
		rep.PerEvent = append(rep.PerEvent, WSEventCount{
			EventSlug: ev, Markets: c, Tokens: len(tokensPerEvent[ev]),
		})
	}
	sort.Slice(rep.PerEvent, func(i, j int) bool {
		if rep.PerEvent[i].Tokens != rep.PerEvent[j].Tokens {
			return rep.PerEvent[i].Tokens > rep.PerEvent[j].Tokens
		}
		return rep.PerEvent[i].EventSlug < rep.PerEvent[j].EventSlug
	})
	// Prediction buckets present? Only if any bucket name still
	// contains "prediction" — which the new naming never produces.
	for name := range rep.PerBucket {
		if strings.Contains(strings.ToLower(name), "prediction") {
			rep.PredictionBucketsPresent = true
			break
		}
	}
	return rep, nil
}

func printWSSelectorReport(out *os.File, r *WSSelectorReport, showTokens bool) {
	fmt.Fprintf(out, "WS selector — mode=%s, limit_markets=%d, generated %s\n",
		r.Mode, r.LimitMarkets, r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "annotation_lookback=%s buckets_used=%s\n",
		r.AnnotationLookback, strings.Join(r.BucketsUsed, ","))

	fmt.Fprintf(out, "\nSummary:\n")
	fmt.Fprintf(out, "  selected markets:                  %d\n", r.UniqueMarkets)
	fmt.Fprintf(out, "  selected tokens:                   %d\n", r.UniqueTokens)
	fmt.Fprintf(out, "  unique events:                     %d\n", r.UniqueEvents)
	fmt.Fprintf(out, "  max tokens per market:             %d\n", r.MaxTokensPerMarket)
	fmt.Fprintf(out, "  markets with >2 tokens:            %d\n", r.MarketsWithMoreThan2Tk)
	fmt.Fprintf(out, "  total token expansion:             %d\n", r.TotalTokenExpansion)
	fmt.Fprintf(out, "  dropped tokens:                    %d\n", r.DroppedTokens)
	fmt.Fprintf(out, "  prediction buckets present:        %s\n", yesNo(r.PredictionBucketsPresent))

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

	if showTokens && len(r.Markets) > 0 {
		fmt.Fprintln(out, "\nPer-market token expansion (full list, no truncation):")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  rank_market\tbucket\tevent_slug\tmarket_slug\ttoken_count\ttokens (idx outcome shortened_id)")
		for _, m := range r.Markets {
			pairs := make([]string, 0, len(m.Tokens))
			for _, t := range m.Tokens {
				tk := t.TokenID
				if len(tk) > 14 {
					tk = tk[:14] + "…"
				}
				outc := t.Outcome
				if outc == "" {
					outc = "?"
				}
				pairs = append(pairs, fmt.Sprintf("[%d %s %s]", t.Index, outc, tk))
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%d\t%s\n",
				m.RankMarket, m.BucketName, m.EventSlug, m.MarketSlug, m.TokenCount, strings.Join(pairs, " "))
		}
		_ = tw.Flush()
	}

	if len(r.PerEvent) > 0 {
		topN := 20
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

	fmt.Fprintln(out, "\nUser flow / AI / writes: none (read-only).")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
