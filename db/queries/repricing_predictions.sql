-- name: UpsertRepricingSignal :exec
INSERT INTO polymarket_repricing_signals (
    event_slug, condition_id, outcome, annotation_hash,
    annotation_time, annotation_title,
    price_before, price_after, annotation_price_change,
    current_price, current_vs_price_after, drift_since_annotation,
    pre_annotation_flow_usd, post_annotation_flow_usd,
    same_side_post_flow_usd, opposite_side_post_flow_usd,
    flow_timing, repricing_status, confidence, explanation,
    updated_at
) VALUES (
    @event_slug, @condition_id, @outcome, @annotation_hash,
    @annotation_time, @annotation_title,
    @price_before, @price_after, @annotation_price_change,
    @current_price, @current_vs_price_after, @drift_since_annotation,
    @pre_annotation_flow_usd, @post_annotation_flow_usd,
    @same_side_post_flow_usd, @opposite_side_post_flow_usd,
    @flow_timing, @repricing_status, @confidence, @explanation,
    NOW()
)
ON CONFLICT (event_slug, condition_id, annotation_hash) DO UPDATE SET
    outcome                     = COALESCE(NULLIF(EXCLUDED.outcome, ''), polymarket_repricing_signals.outcome),
    annotation_time             = COALESCE(EXCLUDED.annotation_time, polymarket_repricing_signals.annotation_time),
    annotation_title            = COALESCE(NULLIF(EXCLUDED.annotation_title, ''), polymarket_repricing_signals.annotation_title),
    price_before                = COALESCE(EXCLUDED.price_before, polymarket_repricing_signals.price_before),
    price_after                 = COALESCE(EXCLUDED.price_after, polymarket_repricing_signals.price_after),
    annotation_price_change     = COALESCE(EXCLUDED.annotation_price_change, polymarket_repricing_signals.annotation_price_change),
    current_price               = COALESCE(EXCLUDED.current_price, polymarket_repricing_signals.current_price),
    current_vs_price_after      = EXCLUDED.current_vs_price_after,
    drift_since_annotation      = EXCLUDED.drift_since_annotation,
    pre_annotation_flow_usd     = EXCLUDED.pre_annotation_flow_usd,
    post_annotation_flow_usd    = EXCLUDED.post_annotation_flow_usd,
    same_side_post_flow_usd     = EXCLUDED.same_side_post_flow_usd,
    opposite_side_post_flow_usd = EXCLUDED.opposite_side_post_flow_usd,
    flow_timing                 = EXCLUDED.flow_timing,
    repricing_status            = EXCLUDED.repricing_status,
    confidence                  = EXCLUDED.confidence,
    explanation                 = COALESCE(NULLIF(EXCLUDED.explanation, ''), polymarket_repricing_signals.explanation),
    updated_at                  = NOW();

-- name: ListRepricingSignalsForEvent :many
SELECT
    id, event_slug, condition_id, outcome, annotation_hash,
    annotation_time, annotation_title,
    price_before, price_after, annotation_price_change,
    current_price, current_vs_price_after, drift_since_annotation,
    pre_annotation_flow_usd, post_annotation_flow_usd,
    same_side_post_flow_usd, opposite_side_post_flow_usd,
    flow_timing, repricing_status, confidence, explanation,
    created_at, updated_at
FROM polymarket_repricing_signals
WHERE event_slug = @event_slug
ORDER BY annotation_time DESC NULLS LAST, id DESC
LIMIT @limit_count;

-- name: GetMarketPrediction :one
SELECT
    id, event_slug, condition_id, outcome, side_bias, summary,
    current_state, state_reason,
    previous_prediction_id, supersedes_prediction_id,
    last_repriced_at, last_confirmed_by_alert_at, last_contradicted_by_alert_at,
    confidence, created_at, updated_at, last_evolved_at
FROM polymarket_market_predictions
WHERE event_slug = @event_slug AND condition_id = @condition_id;

-- name: UpsertMarketPrediction :one
INSERT INTO polymarket_market_predictions (
    event_slug, condition_id, outcome, side_bias, summary,
    current_state, state_reason, confidence,
    last_repriced_at, last_confirmed_by_alert_at, last_contradicted_by_alert_at,
    updated_at
) VALUES (
    @event_slug, @condition_id, @outcome, @side_bias, @summary,
    @current_state, @state_reason, @confidence,
    @last_repriced_at, @last_confirmed_by_alert_at, @last_contradicted_by_alert_at,
    NOW()
)
ON CONFLICT (event_slug, condition_id) DO UPDATE SET
    outcome                       = COALESCE(NULLIF(EXCLUDED.outcome, ''), polymarket_market_predictions.outcome),
    side_bias                     = COALESCE(NULLIF(EXCLUDED.side_bias, ''), polymarket_market_predictions.side_bias),
    summary                       = COALESCE(NULLIF(EXCLUDED.summary, ''), polymarket_market_predictions.summary),
    current_state                 = EXCLUDED.current_state,
    state_reason                  = COALESCE(NULLIF(EXCLUDED.state_reason, ''), polymarket_market_predictions.state_reason),
    confidence                    = GREATEST(EXCLUDED.confidence, polymarket_market_predictions.confidence),
    last_repriced_at              = COALESCE(EXCLUDED.last_repriced_at, polymarket_market_predictions.last_repriced_at),
    last_confirmed_by_alert_at    = COALESCE(EXCLUDED.last_confirmed_by_alert_at, polymarket_market_predictions.last_confirmed_by_alert_at),
    last_contradicted_by_alert_at = COALESCE(EXCLUDED.last_contradicted_by_alert_at, polymarket_market_predictions.last_contradicted_by_alert_at),
    updated_at                    = NOW()
RETURNING id;

-- name: InsertMarketPredictionStateTransition :exec
INSERT INTO polymarket_market_prediction_states (
    prediction_id, previous_state, new_state, reason, evidence_json
) VALUES (
    @prediction_id, @previous_state, @new_state, @reason, @evidence_json
);

-- name: ListMarketPredictionStates :many
SELECT
    id, prediction_id, previous_state, new_state, reason, evidence_json, created_at
FROM polymarket_market_prediction_states
WHERE prediction_id = @prediction_id
ORDER BY created_at DESC, id DESC
LIMIT @limit_count;

-- name: ListPredictionsForEvolution :many
-- Selection for the evolution worker. Filters out resolved /
-- invalidated rows + rows whose last_evolved_at is fresher than
-- @max_age. Orders by state priority (blocked / catalyst-blocked
-- first, then repricing / confirmed / contradicted / watching /
-- already_priced) so the most operationally relevant predictions
-- get the cycle's compute budget first.
SELECT
    id, event_slug, condition_id, outcome, side_bias, summary,
    current_state, state_reason,
    previous_prediction_id, supersedes_prediction_id,
    last_repriced_at, last_confirmed_by_alert_at, last_contradicted_by_alert_at,
    confidence, created_at, updated_at
FROM polymarket_market_predictions
WHERE current_state NOT IN ('resolved', 'invalidated')
  AND (last_evolved_at IS NULL OR last_evolved_at < @max_age)
ORDER BY
    CASE current_state
        WHEN 'blocked'              THEN 1
        WHEN 'active_catalyst'      THEN 2
        WHEN 'repricing'            THEN 3
        WHEN 'confirmed_by_flow'    THEN 4
        WHEN 'contradicted_by_flow' THEN 5
        WHEN 'new'                  THEN 6
        WHEN 'watching'             THEN 7
        WHEN 'already_priced'       THEN 8
        WHEN 'stale'                THEN 9
        ELSE 99
    END,
    last_evolved_at NULLS FIRST,
    updated_at
LIMIT @limit_count;

-- name: TouchPredictionEvolution :exec
-- Bumps last_evolved_at without touching state/confidence — the
-- worker calls this on EVERY processed prediction so the row drops
-- to the back of the selection queue even when nothing material
-- changed. Decoupled from UpsertMarketPrediction so we avoid the
-- updated_at bump that confuses dashboards.
UPDATE polymarket_market_predictions
SET last_evolved_at = NOW()
WHERE id = @id;

-- name: ApplyPredictionDecay :exec
-- Deterministic decay step: decreases confidence by @delta, clamps
-- to @floor (never below). Always bumps last_evolved_at + updated_at
-- so dashboards can see the decay tick. No state change here — the
-- worker's Decide() decides whether the lower confidence triggers
-- a state transition.
UPDATE polymarket_market_predictions
SET confidence      = GREATEST(@floor, confidence - @delta),
    last_evolved_at = NOW(),
    updated_at      = NOW(),
    state_reason    = @reason
WHERE id = @id;
