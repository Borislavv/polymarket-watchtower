-- name: UpsertPredictionUsefulnessScore :one
-- Single live score per prediction; insert-or-replace.
INSERT INTO polymarket_prediction_usefulness_scores
  (prediction_id, score, components_json, reason, created_at, updated_at)
VALUES (@prediction_id, @score, @components_json, @reason, NOW(), NOW())
ON CONFLICT (prediction_id) DO UPDATE
   SET score           = EXCLUDED.score,
       components_json = EXCLUDED.components_json,
       reason          = EXCLUDED.reason,
       updated_at      = NOW()
RETURNING id;

-- name: GetPredictionUsefulnessScore :one
SELECT prediction_id, score, components_json, reason, created_at, updated_at
FROM polymarket_prediction_usefulness_scores
WHERE prediction_id = @prediction_id;

-- name: ListTopUsefulnessScores :many
-- Operator-ranked top-N for the Grafana panel + future Telegram digests.
SELECT s.prediction_id, s.score, s.reason, s.updated_at,
       p.event_slug, p.condition_id, p.outcome, p.current_state, p.confidence
FROM polymarket_prediction_usefulness_scores s
JOIN polymarket_market_predictions p ON p.id = s.prediction_id
WHERE p.current_state NOT IN ('resolved','invalidated')
ORDER BY s.score DESC, s.updated_at DESC
LIMIT @limit_count;

-- name: UpsertPredictionFeedback :exec
-- One row per (prediction_id, horizon). Re-runs with the same key
-- update the prior measurement (the worker may revise as more
-- trades land).
INSERT INTO polymarket_prediction_feedback
  (prediction_id, horizon, price_at_prediction, price_at_horizon,
   price_delta, direction_correct, state_at_horizon,
   repricing_status_at_horizon, catalyst_status_at_horizon,
   flow_confirmed, created_at)
VALUES (@prediction_id, @horizon, @price_at_prediction, @price_at_horizon,
        @price_delta, @direction_correct, @state_at_horizon,
        @repricing_status_at_horizon, @catalyst_status_at_horizon,
        @flow_confirmed, NOW())
ON CONFLICT (prediction_id, horizon) DO UPDATE
   SET price_at_horizon            = EXCLUDED.price_at_horizon,
       price_delta                 = EXCLUDED.price_delta,
       direction_correct           = EXCLUDED.direction_correct,
       state_at_horizon            = EXCLUDED.state_at_horizon,
       repricing_status_at_horizon = EXCLUDED.repricing_status_at_horizon,
       catalyst_status_at_horizon  = EXCLUDED.catalyst_status_at_horizon,
       flow_confirmed              = EXCLUDED.flow_confirmed;

-- name: ListPredictionsForFeedback :many
-- Selection query for the feedback worker. Returns active predictions
-- older than the smallest horizon, paired with whichever horizon
-- rows are missing. The worker filters per-horizon afterwards.
SELECT p.id, p.event_slug, p.condition_id, p.outcome, p.side_bias,
       p.confidence, p.current_state, p.created_at
FROM polymarket_market_predictions p
WHERE p.current_state NOT IN ('resolved','invalidated')
  AND p.created_at < @oldest_eligible
ORDER BY p.created_at ASC
LIMIT @limit_count;

-- name: ListHorizonsRecorded :many
-- Reports which horizons already have a feedback row for the given
-- prediction. The worker subtracts this set from the configured
-- horizons list to find the work it still owes.
SELECT horizon
FROM polymarket_prediction_feedback
WHERE prediction_id = @prediction_id;
