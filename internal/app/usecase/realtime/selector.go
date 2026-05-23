package realtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/ws"
)

// PostgresSelector builds the deterministic subscription set from
// live DB state. Modes:
//
//	hot           — active high-usefulness predictions + recent
//	                alerts + active catalysts (default).
//	predictions   — active predictions only.
//	alerts        — recent alerts only.
//	all_active_limited — every active prediction, capped at
//	                MaxMarkets.
//	off           — empty set (Run short-circuits earlier).
//
// The query always JOINs polymarket_event_page_markets to pull the
// CLOBTokenIDs + OutcomeByToken so the WS client can subscribe by
// asset id without re-querying the mapper per cycle.
type PostgresSelector struct {
	pool *pgxpool.Pool
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
	sql := buildSelectorSQL(mode)
	rows, err := s.pool.Query(ctx, sql, int32(maxMarkets))
	if err != nil {
		return ws.SubscriptionSet{}, fmt.Errorf("selector query: %w", err)
	}
	defer rows.Close()

	// Coalesce per condition_id — one market may emit multiple
	// rows (one per outcome) and we want a single subscription per
	// market with all token ids.
	byCondition := map[string]*ws.MarketSubscription{}
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
		}
		sub.CLOBTokenIDs = append(sub.CLOBTokenIDs, tokenID)
		if outcome != "" {
			sub.OutcomeByToken[tokenID] = outcome
		}
	}
	set := ws.SubscriptionSet{Markets: make([]ws.MarketSubscription, 0, len(byCondition))}
	for _, sub := range byCondition {
		set.Markets = append(set.Markets, *sub)
	}
	return set, nil
}

// buildSelectorSQL returns the SQL for the mode. We hand-roll it
// (instead of sqlc'ing) because the per-mode CTE shape is operator-
// readable + we don't need typed Params.
//
// IMPORTANT: every variant LIMITs by $1 so we never subscribe to
// thousands of markets blindly.
//
// Pairing tokens with outcomes uses LATERAL `WITH ORDINALITY` on both
// JSONB arrays joined by position. We do NOT call set-returning
// functions (unnest, jsonb_array_elements_text) inside COALESCE or
// CASE — Postgres rejects that as SQLSTATE 0A000.
func buildSelectorSQL(mode string) string {
	common := `
SELECT m.event_slug,
       m.condition_id,
       m.market_slug,
       tok.token AS clob_token_id,
       COALESCE(out.outcome, '') AS outcome
FROM (
    SELECT em.event_slug,
           em.condition_id,
           em.market_slug,
           em.clob_token_ids_json,
           em.outcomes_json,
           em.created_at
    FROM polymarket_event_page_markets em
    WHERE em.created_at > NOW() - INTERVAL '24 hours'
      AND em.active = TRUE
    ORDER BY em.created_at DESC
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
	case "predictions":
		return `
WITH active_preds AS (
    SELECT DISTINCT event_slug, condition_id
    FROM polymarket_market_predictions
    WHERE current_state NOT IN ('resolved','invalidated','stale')
      AND archived_at IS NULL
    LIMIT $1
)
` + common + `
WHERE (m.event_slug, m.condition_id) IN (SELECT event_slug, condition_id FROM active_preds)
LIMIT $1
`
	case "alerts":
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
LIMIT $1
`
	case "all_active_limited":
		return common + `
WHERE EXISTS (
    SELECT 1 FROM polymarket_market_predictions p
    WHERE p.event_slug = m.event_slug AND p.condition_id = m.condition_id
      AND p.current_state NOT IN ('resolved','invalidated','stale')
      AND p.archived_at IS NULL
)
LIMIT $1
`
	}
	// "hot" — default. v10.6 expanded to cover the full prioritised
	// signal set the spec asks for:
	//
	//   priority 1: blocked / active_catalyst predictions (catalyst-
	//               adjacent, repricing imminent).
	//   priority 2: active non-stale predictions (watching /
	//               confirmed_by_flow / etc.).
	//   priority 3: recent alerts (24h) — recipient already engaged.
	//   priority 4: active/expected event catalysts.
	//   priority 5: fresh repricing signals (24h, flow_timing in
	//               {pre_event_positioning, post_event_chasing} OR
	//               status in {underreacting, overreacting,
	//               still_repricing}).
	//   priority 6: events with annotations in last 12h.
	//
	// The outer SELECT ORDER BY priority ASC + LIMIT $1 — so when the
	// candidate set exceeds the cap, we keep the highest-priority
	// rows. Dedup by (event_slug, condition_id) via DISTINCT ON.
	return `
WITH hot_set AS (
    SELECT event_slug, condition_id, 1 AS priority
    FROM polymarket_market_predictions
    WHERE current_state IN ('blocked','active_catalyst')
      AND archived_at IS NULL

    UNION ALL
    SELECT event_slug, condition_id, 2 AS priority
    FROM polymarket_market_predictions
    WHERE current_state NOT IN ('resolved','invalidated','stale','blocked','active_catalyst')
      AND archived_at IS NULL

    UNION ALL
    SELECT pm.event_slug, pm.condition_id, 3 AS priority
    FROM polymarket_alerts a
    JOIN polymarket_markets pm ON pm.id = a.market_id
    WHERE a.created_at > NOW() - INTERVAL '24 hours'

    UNION ALL
    SELECT pm.event_slug, pm.condition_id, 4 AS priority
    FROM polymarket_event_catalysts c
    JOIN polymarket_markets pm ON pm.event_slug = c.event_slug
    WHERE c.status IN ('active','expected')

    UNION ALL
    SELECT event_slug, condition_id, 5 AS priority
    FROM polymarket_repricing_signals
    WHERE created_at > NOW() - INTERVAL '24 hours'
      AND (
          repricing_status IN ('underreacting','overreacting','still_repricing','reversed')
          OR flow_timing IN ('pre_event_positioning','post_event_chasing')
      )

    UNION ALL
    SELECT DISTINCT pm.event_slug, pm.condition_id, 6 AS priority
    FROM polymarket_event_annotations an
    JOIN polymarket_markets pm ON pm.event_slug = an.event_slug
    WHERE an.timestamp > NOW() - INTERVAL '12 hours'
),
ranked AS (
    SELECT event_slug, condition_id, MIN(priority) AS priority
    FROM hot_set
    GROUP BY event_slug, condition_id
    ORDER BY priority ASC
    LIMIT $1
)
` + common + `
WHERE (m.event_slug, m.condition_id) IN (SELECT event_slug, condition_id FROM ranked)
LIMIT $1
`
}
