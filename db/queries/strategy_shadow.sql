-- v11.5 Strategy Learning Loop queries (Wave P0).

-- =========================================================================
-- Shadow decisions
-- =========================================================================

-- name: InsertStrategyShadowDecision :one
INSERT INTO polymarket_strategy_shadow_decisions (
    strategy_name, strategy_version, condition_id, event_slug, wallet,
    cohort_id, side, decision_kind, decision_level, score, confidence,
    reasons_json, features_json, shadow_only, linked_alert_dedup_key,
    control_bucket_key
) VALUES (
    @strategy_name, @strategy_version, @condition_id, @event_slug, @wallet,
    @cohort_id, @side, @decision_kind, @decision_level, @score, @confidence,
    @reasons_json, @features_json, @shadow_only, @linked_alert_dedup_key,
    @control_bucket_key
)
RETURNING id;

-- name: ListShadowDecisionsByStrategy :many
SELECT id, strategy_name, strategy_version, condition_id, event_slug,
       wallet, cohort_id, side, decision_kind, decision_level, score,
       confidence, reasons_json, features_json, shadow_only, fired_at,
       linked_alert_dedup_key, control_bucket_key, clv_15m, clv_1h,
       clv_6h, clv_24h, outcome_status, created_at
FROM polymarket_strategy_shadow_decisions
WHERE strategy_name = @strategy_name AND fired_at >= @since
ORDER BY fired_at DESC
LIMIT @row_limit;

-- =========================================================================
-- Market links
-- =========================================================================

-- name: UpsertMarketLink :exec
INSERT INTO polymarket_market_links (
    src_condition_id, dst_condition_id, link_type, direction,
    confidence, event_slug, series_id, link_version, updated_at
) VALUES (
    @src_condition_id, @dst_condition_id, @link_type, @direction,
    @confidence, @event_slug, @series_id, @link_version, NOW()
)
ON CONFLICT (src_condition_id, dst_condition_id, link_type, link_version) DO UPDATE
SET direction  = EXCLUDED.direction,
    confidence = EXCLUDED.confidence,
    event_slug = EXCLUDED.event_slug,
    series_id  = EXCLUDED.series_id,
    updated_at = NOW();

-- name: ListMarketLinksBySource :many
SELECT id, src_condition_id, dst_condition_id, link_type, direction,
       confidence, event_slug, series_id, link_version, created_at, updated_at
FROM polymarket_market_links
WHERE src_condition_id = @src_condition_id
  AND link_version = @link_version
ORDER BY confidence DESC, link_type ASC;

-- =========================================================================
-- Holder snapshots
-- =========================================================================

-- name: UpsertHolderSnapshot :exec
INSERT INTO polymarket_holder_snapshots (
    condition_id, outcome_token, snapshot_at, wallet, rank, shares,
    notional_usd, pct_oi, total_oi, raw_json
) VALUES (
    @condition_id, @outcome_token, @snapshot_at, @wallet, @rank, @shares,
    @notional_usd, @pct_oi, @total_oi, @raw_json
)
ON CONFLICT (condition_id, outcome_token, snapshot_at, wallet) DO UPDATE
SET rank         = EXCLUDED.rank,
    shares       = EXCLUDED.shares,
    notional_usd = EXCLUDED.notional_usd,
    pct_oi       = EXCLUDED.pct_oi,
    total_oi     = EXCLUDED.total_oi,
    raw_json     = EXCLUDED.raw_json;

-- name: LatestHolderSnapshot :one
SELECT id, condition_id, outcome_token, snapshot_at, wallet, rank,
       shares, notional_usd, pct_oi, total_oi, created_at
FROM polymarket_holder_snapshots
WHERE condition_id = @condition_id
  AND outcome_token = @outcome_token
  AND wallet = @wallet
ORDER BY snapshot_at DESC
LIMIT 1;

-- =========================================================================
-- Repricing windows
-- =========================================================================

-- name: InsertRepricingWindow :one
INSERT INTO polymarket_repricing_windows (
    condition_id, event_slug, trigger_kind, trigger_ref,
    opened_at, closes_at, expected_impact_min, expected_impact_max,
    side_bias, baseline_price
) VALUES (
    @condition_id, @event_slug, @trigger_kind, @trigger_ref,
    @opened_at, @closes_at, @expected_impact_min, @expected_impact_max,
    @side_bias, @baseline_price
)
RETURNING id;

-- name: CloseRepricingWindow :exec
UPDATE polymarket_repricing_windows
SET status         = @status,
    resolved_at    = NOW(),
    observed_move  = @observed_move,
    peer_move      = @peer_move,
    lag_score      = @lag_score,
    notes          = @notes
WHERE id = @id;

-- =========================================================================
-- Market risk scores
-- =========================================================================

-- name: UpsertMarketRiskScore :exec
INSERT INTO polymarket_market_risk_scores (
    condition_id, score_version, ambiguity_score, dispute_risk,
    reasons_json, is_active
) VALUES (
    @condition_id, @score_version, @ambiguity_score, @dispute_risk,
    @reasons_json, TRUE
)
ON CONFLICT (condition_id, score_version) DO UPDATE
SET ambiguity_score = EXCLUDED.ambiguity_score,
    dispute_risk    = EXCLUDED.dispute_risk,
    reasons_json    = EXCLUDED.reasons_json,
    is_active       = TRUE,
    computed_at     = NOW();

-- name: ActiveMarketRiskScore :one
SELECT condition_id, score_version, ambiguity_score, dispute_risk,
       reasons_json, computed_at
FROM polymarket_market_risk_scores
WHERE condition_id = @condition_id AND is_active = TRUE
ORDER BY computed_at DESC
LIMIT 1;

-- v11.6 PART 6 — shadow value evaluator queries.

-- name: ListPendingValueRows :many
SELECT id, strategy_name, condition_id,
       COALESCE(event_slug, '') AS event_slug,
       COALESCE(wallet, '')     AS wallet,
       COALESCE(side, '')       AS side,
       fired_at,
       clv_15m, clv_1h, clv_6h, clv_24h
FROM polymarket_strategy_shadow_decisions
WHERE (clv_15m IS NULL OR clv_1h IS NULL OR clv_6h IS NULL OR clv_24h IS NULL)
  AND fired_at > @max_age
ORDER BY fired_at ASC
LIMIT @row_limit;

-- name: UpdateShadowDecisionValues :exec
UPDATE polymarket_strategy_shadow_decisions
SET clv_15m  = COALESCE(clv_15m,  sqlc.narg('clv_15m')::DOUBLE PRECISION),
    clv_1h   = COALESCE(clv_1h,   sqlc.narg('clv_1h')::DOUBLE PRECISION),
    clv_6h   = COALESCE(clv_6h,   sqlc.narg('clv_6h')::DOUBLE PRECISION),
    clv_24h  = COALESCE(clv_24h,  sqlc.narg('clv_24h')::DOUBLE PRECISION),
    outcome_status = COALESCE(outcome_status, sqlc.narg('outcome_status')::TEXT)
WHERE id = @id;

-- v11.6 PART 7 — promotion review queries.

-- name: InsertStrategyPromotionReview :exec
INSERT INTO polymarket_strategy_promotion_reviews (
    strategy_name, strategy_version, sample_size,
    median_signed_move_6h, reversal_15m_ratio, alerts_per_day,
    eligible, reasons_json, bucket_diagnostics, reviewed_at
) VALUES (
    @strategy_name, @strategy_version, @sample_size,
    @median_signed_move_6h, @reversal_15m_ratio, @alerts_per_day,
    @eligible, @reasons_json, @bucket_diagnostics, @reviewed_at
);

-- v11.10 PART 7 — bucketed promotion samples (decision_level dim).
-- Per-(strategy, version, decision_level) sub-aggregate. The Go layer
-- joins this with AggregatePromotionSamplesByLinkage to assemble the
-- bucket_diagnostics JSONB column on the review row.

-- name: AggregatePromotionSamplesByDecisionLevel :many
SELECT strategy_name,
       COALESCE(strategy_version, '') AS strategy_version,
       COALESCE(decision_level, 'unknown') AS bucket_key,
       COUNT(*)::INTEGER AS sample_size,
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY clv_6h), 0)::DOUBLE PRECISION AS median_signed_move_6h,
       COALESCE(AVG(CASE WHEN clv_15m IS NOT NULL AND clv_15m > 0 AND clv_1h IS NOT NULL AND clv_1h <= 0 THEN 1.0 ELSE 0.0 END), 0)::DOUBLE PRECISION AS reversal_15m_ratio,
       (COUNT(*)::DOUBLE PRECISION / GREATEST(EXTRACT(EPOCH FROM (NOW() - MIN(fired_at))) / 86400, 1))::DOUBLE PRECISION AS alerts_per_day
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= @lookback_start
  AND COALESCE(strategy_version, '') NOT ILIKE '%integration%'
  AND COALESCE(strategy_version, '') NOT ILIKE '%probe%'
  AND COALESCE(strategy_version, '') NOT ILIKE '%test%'
  AND clv_6h IS NOT NULL
GROUP BY strategy_name, strategy_version, decision_level
ORDER BY strategy_name, strategy_version, bucket_key;

-- name: AggregatePromotionSamplesByLinkage :many
SELECT strategy_name,
       COALESCE(strategy_version, '') AS strategy_version,
       CASE WHEN linked_alert_dedup_key IS NULL OR linked_alert_dedup_key = ''
            THEN 'standalone' ELSE 'linked' END AS bucket_key,
       COUNT(*)::INTEGER AS sample_size,
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY clv_6h), 0)::DOUBLE PRECISION AS median_signed_move_6h,
       COALESCE(AVG(CASE WHEN clv_15m IS NOT NULL AND clv_15m > 0 AND clv_1h IS NOT NULL AND clv_1h <= 0 THEN 1.0 ELSE 0.0 END), 0)::DOUBLE PRECISION AS reversal_15m_ratio,
       (COUNT(*)::DOUBLE PRECISION / GREATEST(EXTRACT(EPOCH FROM (NOW() - MIN(fired_at))) / 86400, 1))::DOUBLE PRECISION AS alerts_per_day
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= @lookback_start
  AND COALESCE(strategy_version, '') NOT ILIKE '%integration%'
  AND COALESCE(strategy_version, '') NOT ILIKE '%probe%'
  AND COALESCE(strategy_version, '') NOT ILIKE '%test%'
  AND clv_6h IS NOT NULL
GROUP BY strategy_name, strategy_version, bucket_key
ORDER BY strategy_name, strategy_version, bucket_key;

-- name: AggregatePromotionSamples :many
SELECT strategy_name,
       COALESCE(strategy_version, '') AS strategy_version,
       COUNT(*)::INTEGER AS sample_size,
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY clv_6h), 0)::DOUBLE PRECISION AS median_signed_move_6h,
       COALESCE(AVG(CASE WHEN clv_15m IS NOT NULL AND clv_15m > 0 AND clv_1h IS NOT NULL AND clv_1h <= 0 THEN 1.0 ELSE 0.0 END), 0)::DOUBLE PRECISION AS reversal_15m_ratio,
       (COUNT(*)::DOUBLE PRECISION / GREATEST(EXTRACT(EPOCH FROM (NOW() - MIN(fired_at))) / 86400, 1))::DOUBLE PRECISION AS alerts_per_day
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= @lookback_start
  -- v11.8: exclude probe/test/integration rows so promotion never
  -- promotes a strategy that only ever saw synthetic data.
  AND COALESCE(strategy_version, '') NOT ILIKE '%integration%'
  AND COALESCE(strategy_version, '') NOT ILIKE '%probe%'
  AND COALESCE(strategy_version, '') NOT ILIKE '%test%'
  -- require at least one CLV column to be filled so promotion never
  -- runs on rows the value evaluator hasn't touched yet.
  AND clv_6h IS NOT NULL
GROUP BY strategy_name, strategy_version;

-- v11.7 PART 2 — marketlinks Builder production source.
-- Returns market sets grouped by event_slug, capped by limit.

-- name: ListEventGroupedMarkets :many
SELECT COALESCE(event_slug, '')::TEXT AS event_slug,
       array_agg(condition_id ORDER BY id ASC)::text[] AS condition_ids
FROM polymarket_markets
WHERE event_slug IS NOT NULL AND event_slug <> ''
  AND deleted_at IS NULL
  AND purged_at IS NULL
GROUP BY event_slug
HAVING COUNT(*) >= 2
ORDER BY MAX(updated_at) DESC
LIMIT @row_limit;

-- v11.7 PART 6 — walletgraph production source.
-- Returns recent (wallet, event_slug, side) tuples bounded by lookback.

-- name: ListWalletCoTradeRows :many
SELECT COALESCE(tr.wallet_address, '')             AS wallet,
       COALESCE(m.event_slug, '')                  AS event_slug,
       t.side::text                                AS side,
       t.traded_at
FROM polymarket_trades t
JOIN polymarket_traders tr ON tr.id = t.trader_id
JOIN polymarket_markets m ON m.id = t.market_id
WHERE t.traded_at >= @since
  AND tr.wallet_address IS NOT NULL
  AND tr.wallet_address <> ''
  AND m.event_slug IS NOT NULL
  AND m.event_slug <> ''
ORDER BY t.traded_at ASC
LIMIT @row_limit;

-- v11.7 PART 4 — riskscore production source.
-- Returns market facts in bounded batches, ordered by stale-first.

-- name: ListRiskScoreCandidates :many
SELECT m.id, m.condition_id, m.question,
       COALESCE(m.event_title, '') AS event_title,
       COALESCE(rs.computed_at, '1970-01-01'::timestamptz) AS last_computed
FROM polymarket_markets m
LEFT JOIN polymarket_market_risk_scores rs
  ON rs.condition_id = m.condition_id AND rs.is_active = TRUE
WHERE m.deleted_at IS NULL AND m.purged_at IS NULL
ORDER BY last_computed ASC, m.id DESC
LIMIT @row_limit;

-- v11.7 PART 5 — repricing trigger source.
-- New unprocessed catalyst rows over the recent lookback window.

-- name: ListRepricingTriggers :many
SELECT c.id, c.event_slug, c.title,
       COALESCE(c.catalyst_type, 'generic') AS catalyst_type,
       c.expected_at,
       COALESCE(c.confidence, 0.5)::DOUBLE PRECISION AS confidence
FROM polymarket_event_catalysts c
WHERE c.expected_at >= @since
  AND c.status IN ('expected', 'active')
ORDER BY c.expected_at DESC
LIMIT @row_limit;

-- v11.7 PART 10 — outcome_status backfill.
-- Returns shadow rows linked to alerts that have a terminal outcome.

-- name: ListShadowRowsForOutcomeBackfill :many
SELECT s.id, s.linked_alert_dedup_key,
       COALESCE(a.outcome_status, '') AS alert_outcome
FROM polymarket_strategy_shadow_decisions s
JOIN polymarket_alerts a ON a.dedup_key = s.linked_alert_dedup_key
WHERE s.outcome_status IS NULL
  AND s.linked_alert_dedup_key IS NOT NULL
  AND s.linked_alert_dedup_key <> ''
  AND a.outcome_status IS NOT NULL
  AND a.outcome_status NOT IN ('pending', '')
LIMIT @row_limit;

-- name: UpdateShadowOutcomeStatus :exec
UPDATE polymarket_strategy_shadow_decisions
SET outcome_status = @outcome_status
WHERE id = @id AND outcome_status IS NULL;

-- v11.8 PART 5 — staged input readers.

-- name: ListMarketLinksForEvent :many
SELECT src_condition_id, dst_condition_id, link_type, direction, confidence, event_slug
FROM polymarket_market_links
WHERE event_slug = @event_slug AND link_version = @link_version
ORDER BY confidence DESC
LIMIT @row_limit;

-- name: ListCatalystsForEvent :many
SELECT event_slug, COALESCE(catalyst_type, 'generic') AS catalyst_type,
       title, expected_at,
       COALESCE(confidence, 0.5)::DOUBLE PRECISION AS confidence,
       status
FROM polymarket_event_catalysts
WHERE event_slug = @event_slug
  AND status IN ('expected', 'active')
ORDER BY expected_at ASC NULLS LAST
LIMIT @row_limit;

-- name: GetActiveRiskScoreForCondition :one
SELECT condition_id, ambiguity_score, dispute_risk, reasons_json, computed_at
FROM polymarket_market_risk_scores
WHERE condition_id = @condition_id AND is_active = TRUE
ORDER BY computed_at DESC
LIMIT 1;

-- name: ListWalletEdgesForWallet :many
SELECT wallet_a, wallet_b, edge_kind, similarity_score, co_events_count, last_seen_at
FROM polymarket_wallet_graph_edges
WHERE (wallet_a = @wallet OR wallet_b = @wallet)
  AND edge_version = @edge_version
ORDER BY similarity_score DESC
LIMIT @row_limit;

-- name: ListClosedRepricingWindowsForCondition :many
SELECT id, condition_id, event_slug, trigger_kind, trigger_ref, opened_at, closes_at,
       status, observed_move, peer_move, lag_score
FROM polymarket_repricing_windows
WHERE condition_id = @condition_id
  AND status IN ('closed_lag_detected', 'closed_no_lag')
  AND opened_at >= @since
ORDER BY opened_at DESC
LIMIT @row_limit;

-- name: ListRecentShadowDecisionsForCondition :many
SELECT id, strategy_name, side, decision_kind, decision_level, score, confidence, fired_at
FROM polymarket_strategy_shadow_decisions
WHERE condition_id = @condition_id
  AND fired_at >= @since
ORDER BY fired_at DESC
LIMIT @row_limit;

-- name: ListStandaloneShadowRowsForOutcomeBackfill :many
SELECT s.id, s.condition_id, COALESCE(m.closed, FALSE) AS market_closed
FROM polymarket_strategy_shadow_decisions s
LEFT JOIN polymarket_markets m ON m.condition_id = s.condition_id
WHERE s.outcome_status IS NULL
  AND (s.linked_alert_dedup_key IS NULL OR s.linked_alert_dedup_key = '')
  AND m.closed = TRUE
LIMIT @row_limit;

-- v11.9 PART 4 — repricing close-phase price sampler.

-- name: ListOpenRepricingWindows :many
SELECT id, condition_id, event_slug, trigger_kind, trigger_ref,
       opened_at, closes_at, side_bias, baseline_price
FROM polymarket_repricing_windows
WHERE status = 'open' AND closes_at <= @due_before
ORDER BY closes_at ASC
LIMIT @row_limit;

-- name: FirstTradePriceForCondition :one
-- Returns the first trade price for (condition_id, traded_at >= @at).
-- Used by the repricing close-phase target sampler.
SELECT t.price::DOUBLE PRECISION AS price
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
WHERE m.condition_id = @condition_id
  AND t.traded_at >= @at
ORDER BY t.traded_at ASC
LIMIT 1;

-- name: LastTradePriceForCondition :one
-- Returns the most recent trade price for (condition_id, traded_at <= @at).
SELECT t.price::DOUBLE PRECISION AS price
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
WHERE m.condition_id = @condition_id
  AND t.traded_at <= @at
ORDER BY t.traded_at DESC
LIMIT 1;

-- name: ListPeerConditionsByMarketLinks :many
-- Returns peer condition_ids from polymarket_market_links anchored
-- on @src_condition_id. Bounded by the link_version + a row limit.
SELECT DISTINCT dst_condition_id
FROM polymarket_market_links
WHERE src_condition_id = @src_condition_id
  AND link_version = @link_version
LIMIT @row_limit;

-- v11.9 PART 5 — thesis lines wallet aggregate.

-- name: AggregateWalletThesisLines :many
-- Aggregates per (wallet, condition_id) directional exposure across
-- linked markets within a lookback window. The orchestration layer
-- groups these rows by event_slug to feed thesisaccum.
SELECT COALESCE(tr.wallet_address, '')              AS wallet,
       m.condition_id                               AS condition_id,
       COALESCE(m.event_slug, '')                   AS event_slug,
       t.side::text                                 AS side,
       SUM(t.notional_usd)::DOUBLE PRECISION        AS notional_usd,
       COUNT(*)::INTEGER                            AS trades,
       MAX(t.traded_at)::TIMESTAMPTZ                AS last_traded_at
FROM polymarket_trades t
JOIN polymarket_traders tr ON tr.id = t.trader_id
JOIN polymarket_markets m  ON m.id = t.market_id
WHERE t.traded_at >= @since
  AND tr.wallet_address IS NOT NULL AND tr.wallet_address <> ''
  AND m.event_slug IS NOT NULL AND m.event_slug <> ''
GROUP BY tr.wallet_address, m.condition_id, m.event_slug, t.side
HAVING COUNT(*) >= 2
ORDER BY MAX(t.traded_at) DESC
LIMIT @row_limit;

-- v11.9 PART 6 — outcome resolver per-side.

-- name: ListStandaloneResolvedAlertOutcomes :many
-- Returns shadow rows linked to an alert with a resolved outcome
-- (winning_outcome_token IS NOT NULL). Used by strategyoutcome to
-- compute correct/wrong based on the shadow row's Side.
SELECT s.id,
       COALESCE(s.side, '') AS side,
       COALESCE(a.winning_outcome_token, '') AS winning_token,
       COALESCE(a.winning_outcome_label, '') AS winning_label,
       COALESCE(a.outcome_status, '') AS alert_outcome
FROM polymarket_strategy_shadow_decisions s
JOIN polymarket_alerts a ON a.dedup_key = s.linked_alert_dedup_key
WHERE s.outcome_status IS NULL
  AND s.linked_alert_dedup_key IS NOT NULL
  AND s.linked_alert_dedup_key <> ''
  AND a.winning_outcome_token IS NOT NULL
  AND a.winning_outcome_token <> ''
LIMIT @row_limit;

-- v11.9 wallet thesis lines.

-- name: UpsertWalletThesisLine :exec
INSERT INTO polymarket_wallet_thesis_lines (
    wallet, condition_id, event_slug, side, notional_usd, trades,
    last_traded_at, lookback_hours, refreshed_at
) VALUES (
    @wallet, @condition_id, @event_slug, @side, @notional_usd, @trades,
    @last_traded_at, @lookback_hours, NOW()
)
ON CONFLICT (wallet, condition_id, side, lookback_hours) DO UPDATE
SET notional_usd   = EXCLUDED.notional_usd,
    trades         = EXCLUDED.trades,
    event_slug     = EXCLUDED.event_slug,
    last_traded_at = EXCLUDED.last_traded_at,
    refreshed_at   = NOW();

-- name: ListWalletThesisLinesForEvent :many
SELECT wallet, condition_id, event_slug, side, notional_usd, trades, last_traded_at
FROM polymarket_wallet_thesis_lines
WHERE event_slug = @event_slug
  AND wallet     = @wallet
  AND lookback_hours = @lookback_hours
ORDER BY notional_usd DESC
LIMIT @row_limit;

-- v11.10 book feature bars upsert.

-- name: UpsertBookFeatureBar :exec
INSERT INTO polymarket_book_feature_bars (
    condition_id, outcome_token, bar_seconds, bar_start,
    best_bid, best_ask, mid_price,
    bid_depth_top_n, ask_depth_top_n,
    spread, spread_z,
    bid_depth_delta_pct, ask_depth_delta_pct, mid_delta
) VALUES (
    @condition_id, @outcome_token, @bar_seconds, @bar_start,
    @best_bid, @best_ask, @mid_price,
    @bid_depth_top_n, @ask_depth_top_n,
    @spread, @spread_z,
    @bid_depth_delta_pct, @ask_depth_delta_pct, @mid_delta
)
ON CONFLICT (condition_id, outcome_token, bar_seconds, bar_start) DO UPDATE
SET best_bid            = EXCLUDED.best_bid,
    best_ask            = EXCLUDED.best_ask,
    mid_price           = EXCLUDED.mid_price,
    bid_depth_top_n     = EXCLUDED.bid_depth_top_n,
    ask_depth_top_n     = EXCLUDED.ask_depth_top_n,
    spread              = EXCLUDED.spread,
    spread_z            = EXCLUDED.spread_z,
    bid_depth_delta_pct = EXCLUDED.bid_depth_delta_pct,
    ask_depth_delta_pct = EXCLUDED.ask_depth_delta_pct,
    mid_delta           = EXCLUDED.mid_delta;

-- name: ListBookbarsCandidates :many
-- Active tokens for bookbars worker. Bounded by limit.
SELECT m.condition_id, COALESCE(mo.token_id, '') AS token_id
FROM polymarket_markets m
JOIN polymarket_market_outcomes mo ON mo.market_id = m.id
WHERE m.active = TRUE AND m.closed = FALSE
  AND m.deleted_at IS NULL AND m.purged_at IS NULL
  AND mo.token_id IS NOT NULL AND mo.token_id <> ''
ORDER BY m.last_seen_at DESC NULLS LAST
LIMIT @row_limit;

-- v11.10 PART 6 — worker priority-bucket budgeting.
-- Returns deduped (condition_id, token_id) pairs annotated with their
-- highest-priority bucket. Buckets:
--   1 = operator-pinned (explicit condition_ids array)
--   2 = recent-alert (≤24h)
--   3 = catalyst-near (status active/expected, ≤72h ahead)
--   4 = linked-to-fired (market_links neighbour of any recent alert)
--   5 = liquid (top by Polymarket liquidity, recent event-page snapshot)
--   6 = fallback active (last_seen_at DESC)
-- Each bucket respects its own LIMIT so a fat bucket can't starve the
-- others. Dedupe keeps the MIN(bucket) per condition_id — once a
-- market is operator-pinned it is not double-counted as fallback.

-- name: ListBucketedMarketTokens :many
WITH pinned AS (
    SELECT m.condition_id, 1::int AS bucket
    FROM polymarket_markets m
    WHERE m.deleted_at IS NULL AND m.purged_at IS NULL
      AND m.condition_id = ANY(@pinned_condition_ids::text[])
    LIMIT @pinned_limit::int
),
recent_alert AS (
    SELECT DISTINCT pm.condition_id, 2::int AS bucket
    FROM polymarket_alerts a
    JOIN polymarket_markets pm ON pm.id = a.market_id
    WHERE a.created_at > NOW() - INTERVAL '24 hours'
      AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
    LIMIT @recent_alert_limit::int
),
catalyst_near AS (
    SELECT DISTINCT pm.condition_id, 3::int AS bucket
    FROM polymarket_event_catalysts c
    JOIN polymarket_markets pm ON pm.event_slug = c.event_slug
    WHERE c.status IN ('active','expected')
      AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
      AND (c.expected_at IS NULL OR c.expected_at <= NOW() + INTERVAL '72 hours')
    LIMIT @catalyst_near_limit::int
),
linked_to_fired AS (
    SELECT DISTINCT ml.src_condition_id AS condition_id, 4::int AS bucket
    FROM polymarket_market_links ml
    JOIN polymarket_alerts a ON a.created_at > NOW() - INTERVAL '24 hours'
    JOIN polymarket_markets pm_a ON pm_a.id = a.market_id
    WHERE ml.dst_condition_id = pm_a.condition_id
    UNION
    SELECT DISTINCT ml.dst_condition_id, 4::int AS bucket
    FROM polymarket_market_links ml
    JOIN polymarket_alerts a ON a.created_at > NOW() - INTERVAL '24 hours'
    JOIN polymarket_markets pm_a ON pm_a.id = a.market_id
    WHERE ml.src_condition_id = pm_a.condition_id
    LIMIT @linked_to_fired_limit::int
),
liquid AS (
    SELECT q.condition_id, 5::int AS bucket
    FROM (
        SELECT pm.condition_id, MAX(em.liquidity) AS max_liq
        FROM polymarket_event_page_markets em
        JOIN polymarket_markets pm ON pm.condition_id = em.condition_id
        WHERE em.created_at > NOW() - INTERVAL '24 hours'
          AND em.active = TRUE
          AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
        GROUP BY pm.condition_id
    ) q
    ORDER BY q.max_liq DESC NULLS LAST
    LIMIT @liquid_limit::int
),
fallback_active AS (
    SELECT m.condition_id, 6::int AS bucket
    FROM polymarket_markets m
    WHERE m.active = TRUE AND m.closed = FALSE
      AND m.deleted_at IS NULL AND m.purged_at IS NULL
    ORDER BY m.last_seen_at DESC NULLS LAST
    LIMIT @fallback_limit::int
),
unioned AS (
    SELECT * FROM pinned
    UNION ALL SELECT * FROM recent_alert
    UNION ALL SELECT * FROM catalyst_near
    UNION ALL SELECT * FROM linked_to_fired
    UNION ALL SELECT * FROM liquid
    UNION ALL SELECT * FROM fallback_active
),
dedup AS (
    SELECT condition_id, MIN(bucket) AS bucket
    FROM unioned
    GROUP BY condition_id
)
SELECT d.condition_id::text   AS condition_id,
       COALESCE(mo.token_id, '')::text AS token_id,
       d.bucket::int          AS bucket
FROM dedup d
JOIN polymarket_markets m ON m.condition_id = d.condition_id
JOIN polymarket_market_outcomes mo ON mo.market_id = m.id
WHERE mo.token_id IS NOT NULL AND mo.token_id <> ''
ORDER BY d.bucket ASC, d.condition_id ASC;
