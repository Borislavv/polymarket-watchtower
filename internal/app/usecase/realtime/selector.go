package realtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/ws"
)

// PostgresSelector builds the deterministic WS subscription set from
// live DB state. v11.12 (insider-prior) removed every prediction-state
// input — prediction tables are stale (v11.2 stopped writing) and were
// silently degrading the WS coverage. Modes:
//
//	hot     — the prioritised insider-prior bucket set (default).
//	alerts  — recent alerts only (test/CLI convenience).
//	off     — empty set (Run short-circuits earlier).
//
// The query always JOINs polymarket_event_page_markets to pull the
// CLOBTokenIDs + OutcomeByToken so the WS client can subscribe by
// asset id without re-querying the mapper per cycle.
//
// MARKET-LIMITED, NOT TOKEN-LIMITED:
// the selector caps by MARKETS ($1). Every selected market expands to
// ALL clob_token_ids. The WS client subscribes to the full set and
// only refuses (loudly) if a configurable hard cap is breached. There
// is no silent token truncation anywhere in the pipeline.
//
// New hot buckets (priority 1 = highest):
//
//	1 — operator_pinned          (explicit condition ids)
//	2 — recent_alert             (alerts in last 24h)
//	3 — active_or_expected_catalyst  (catalyst status in {active, expected})
//	4 — repricing_signal         (signals in last 24h, flow_timing or
//	                              repricing_status matches)
//	5 — event_annotation_recent  (annotations in last AnnotationLookback;
//	                              default 168h = 7d)
//	6 — high_trade_market        (opt-in; ≥ HighTradeMinTrades24h trades
//	                              in HighTradeLookbackHours)
//	7 — liquid_active            (high recent liquidity, safety net for
//	                              opt-in operators; currently inert)
//
// Prediction buckets (blocked / active_catalyst / watching_prediction /
// polymarket_market_predictions) have been REMOVED. Pinned by tests in
// selector_test.go::TestBuildSelectorSQL_NoPredictionReferences.
type PostgresSelector struct {
	pool                   *pgxpool.Pool
	IncludeHighTrade       bool
	HighTradeMinTrades24h  int
	HighTradeLookbackHours int
	// AnnotationLookback is the freshness window for the
	// event_annotation_recent bucket. Default 168h = 7d, matching
	// the linkup-source cadence of ~1 entry per event per day.
	AnnotationLookback time.Duration
	// OperatorPinnedConditionIDs always appear in the subscription
	// set (priority 1). Order preserved for stable rank output.
	OperatorPinnedConditionIDs []string
}

func NewPostgresSelector(pool *pgxpool.Pool) *PostgresSelector {
	return &PostgresSelector{pool: pool}
}

// Select implements SelectorFunc. The mode + max-markets pair fully
// determines the SQL that runs; no AI / no Telegram side effects.
func (s *PostgresSelector) Select(ctx context.Context, mode string, maxMarkets int) (ws.SubscriptionSet, error) {
	if mode == "off" || maxMarkets <= 0 {
		return ws.SubscriptionSet{}, nil
	}
	sql, args := buildSelectorSQL(mode, s.selectorOptions(), maxMarkets)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return ws.SubscriptionSet{}, fmt.Errorf("selector query: %w", err)
	}
	defer rows.Close()

	// Coalesce per condition_id — one market may emit multiple
	// rows (one per outcome) and we want a single subscription per
	// market with ALL its token ids. No truncation, no first-N
	// slicing.
	byCondition := map[string]*ws.MarketSubscription{}
	order := make([]string, 0)
	tokenSeen := map[string]map[string]struct{}{}
	for rows.Next() {
		var eventSlug, conditionID, marketSlug, tokenID, outcome string
		if err := rows.Scan(&eventSlug, &conditionID, &marketSlug, &tokenID, &outcome); err != nil {
			continue
		}
		tokenID = strings.TrimSpace(tokenID)
		if conditionID == "" || tokenID == "" {
			continue
		}
		sub, ok := byCondition[conditionID]
		if !ok {
			sub = &ws.MarketSubscription{
				EventSlug:      eventSlug,
				ConditionID:    conditionID,
				MarketSlug:     marketSlug,
				OutcomeByToken: map[string]string{},
			}
			byCondition[conditionID] = sub
			order = append(order, conditionID)
		}
		// Dedup identical token ids — defensive only. ALL distinct
		// tokens for a selected market are kept; nothing is dropped
		// because of a per-market cap.
		seenSet, ok := tokenSeen[conditionID]
		if !ok {
			seenSet = map[string]struct{}{}
			tokenSeen[conditionID] = seenSet
		}
		if _, dup := seenSet[tokenID]; !dup {
			seenSet[tokenID] = struct{}{}
			sub.CLOBTokenIDs = append(sub.CLOBTokenIDs, tokenID)
		}
		if outcome != "" {
			sub.OutcomeByToken[tokenID] = outcome
		}
	}
	set := ws.SubscriptionSet{Markets: make([]ws.MarketSubscription, 0, len(byCondition))}
	for _, cid := range order {
		set.Markets = append(set.Markets, *byCondition[cid])
	}
	return set, nil
}

// selectorOptions snapshots the runtime knobs the selector reads on
// every Select call. The selector itself is concurrency-safe and
// caller-owned — these reads happen under no lock.
type selectorOptions struct {
	IncludeHighTrade           bool
	HighTradeMinTrades24h      int
	HighTradeLookbackHours     int
	AnnotationLookbackHours    int
	OperatorPinnedConditionIDs []string
}

func (s *PostgresSelector) selectorOptions() selectorOptions {
	opts := selectorOptions{
		IncludeHighTrade:           s.IncludeHighTrade,
		HighTradeMinTrades24h:      s.HighTradeMinTrades24h,
		HighTradeLookbackHours:     s.HighTradeLookbackHours,
		OperatorPinnedConditionIDs: s.OperatorPinnedConditionIDs,
	}
	if opts.HighTradeMinTrades24h <= 0 {
		opts.HighTradeMinTrades24h = 50
	}
	if opts.HighTradeLookbackHours <= 0 {
		opts.HighTradeLookbackHours = 24
	}
	// AnnotationLookback default: 168h = 7d. Live audit
	// (875 annotations, source=linkup, ~1/day per event) shows the
	// previous 12h window caught only 1 event vs 4 events in 7d.
	hours := int(s.AnnotationLookback.Hours())
	if hours <= 0 {
		hours = 168
	}
	opts.AnnotationLookbackHours = hours
	return opts
}

// buildSelectorSQL returns the SQL for the mode + the positional args.
// Pairing tokens with outcomes uses LATERAL `WITH ORDINALITY` on both
// JSONB arrays joined by position. We do NOT call set-returning
// functions (unnest, jsonb_array_elements_text) inside COALESCE or
// CASE — Postgres rejects that as SQLSTATE 0A000.
//
// polymarket_event_page_markets is snapshot-historical — the same
// condition_id can appear dozens of times over 24h. DISTINCT ON
// (em.condition_id) picks the freshest snapshot per market.
//
// $1 = MARKET limit (NEVER a token limit).
// $2 = operator pinned condition_ids array (text[]).
func buildSelectorSQL(mode string, opts selectorOptions, maxMarkets int) (string, []any) {
	common := `
SELECT m.event_slug,
       m.condition_id,
       m.market_slug,
       tok.token AS clob_token_id,
       COALESCE(out.outcome, '') AS outcome
FROM (
    SELECT DISTINCT ON (em.condition_id)
           em.event_slug,
           em.condition_id,
           em.market_slug,
           em.clob_token_ids_json,
           em.outcomes_json,
           em.created_at
    FROM polymarket_event_page_markets em
    WHERE em.created_at > NOW() - INTERVAL '24 hours'
      AND em.active = TRUE
    ORDER BY em.condition_id, em.created_at DESC
) m
CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(m.clob_token_ids_json, '[]'::jsonb))
    WITH ORDINALITY AS tok(token, ord)
LEFT JOIN LATERAL (
    SELECT o.outcome
    FROM jsonb_array_elements_text(COALESCE(m.outcomes_json, '[]'::jsonb))
        WITH ORDINALITY AS o(outcome, ord)
    WHERE o.ord = tok.ord
    LIMIT 1
) AS out ON TRUE
`
	switch mode {
	case "alerts":
		// Test/CLI convenience mode. Production runs in "hot".
		return `
WITH recent_alerts AS (
    SELECT DISTINCT pm.event_slug, pm.condition_id
    FROM polymarket_alerts a
    JOIN polymarket_markets pm ON pm.id = a.market_id
    WHERE a.created_at > NOW() - INTERVAL '24 hours'
    LIMIT $1
)
` + common + `
WHERE (m.event_slug, m.condition_id) IN (SELECT event_slug, condition_id FROM recent_alerts)
`, []any{int32(maxMarkets)}
	}

	// "hot" — default. v11.12-insider-prior bucket scheme. The outer
	// SELECT ORDER BY priority ASC + LIMIT $1 — so when the candidate
	// set exceeds the cap, we keep the highest-priority MARKETS.
	// LIMIT $1 applies to MARKETS, not tokens.
	highTradeCTE := ""
	if opts.IncludeHighTrade {
		highTradeCTE = fmt.Sprintf(`
    UNION ALL
    SELECT pm.event_slug, pm.condition_id, 6 AS priority
    FROM polymarket_markets pm
    JOIN polymarket_trades t ON t.market_id = pm.id
    WHERE pm.active = TRUE AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
      AND t.traded_at >= NOW() - INTERVAL '%d hours'
    GROUP BY pm.event_slug, pm.condition_id
    HAVING COUNT(t.id) >= %d
`, opts.HighTradeLookbackHours, opts.HighTradeMinTrades24h)
	}

	// Operator-pinned bucket: emitted as priority 1 via the $2 array.
	// Empty array → bucket contributes nothing, no rows.
	hot := fmt.Sprintf(`
WITH hot_set AS (
    -- bucket 1: operator_pinned
    SELECT pm.event_slug, pm.condition_id, 1 AS priority
    FROM polymarket_markets pm
    WHERE pm.condition_id = ANY($2::text[])

    UNION ALL
    -- bucket 2: recent_alert (24h)
    SELECT pm.event_slug, pm.condition_id, 2 AS priority
    FROM polymarket_alerts a
    JOIN polymarket_markets pm ON pm.id = a.market_id
    WHERE a.created_at > NOW() - INTERVAL '24 hours'

    UNION ALL
    -- bucket 3: active_or_expected_catalyst
    SELECT pm.event_slug, pm.condition_id, 3 AS priority
    FROM polymarket_event_catalysts c
    JOIN polymarket_markets pm ON pm.event_slug = c.event_slug
    WHERE c.status IN ('active','expected')

    UNION ALL
    -- bucket 4: repricing_signal (24h)
    SELECT event_slug, condition_id, 4 AS priority
    FROM polymarket_repricing_signals
    WHERE created_at > NOW() - INTERVAL '24 hours'
      AND (
          repricing_status IN ('underreacting','overreacting','still_repricing','reversed')
          OR flow_timing IN ('pre_event_positioning','post_event_chasing')
      )

    UNION ALL
    -- bucket 5: event_annotation_recent (default 168h = 7d)
    SELECT DISTINCT pm.event_slug, pm.condition_id, 5 AS priority
    FROM polymarket_event_annotations an
    JOIN polymarket_markets pm ON pm.event_slug = an.event_slug
    WHERE an.timestamp > NOW() - INTERVAL '%d hours'
%s
),
ranked AS (
    SELECT event_slug, condition_id, MIN(priority) AS priority
    FROM hot_set
    GROUP BY event_slug, condition_id
    ORDER BY priority ASC, condition_id ASC
    LIMIT $1
)
`+common+`
WHERE (m.event_slug, m.condition_id) IN (SELECT event_slug, condition_id FROM ranked)
`, opts.AnnotationLookbackHours, highTradeCTE)

	// pgx maps a Go []string to PostgreSQL text[] only when the value
	// is non-nil. Use an empty []string when no pins are configured.
	pins := opts.OperatorPinnedConditionIDs
	if pins == nil {
		pins = []string{}
	}
	return hot, []any{int32(maxMarkets), pins}
}
