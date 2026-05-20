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
    confidence, created_at, updated_at
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
