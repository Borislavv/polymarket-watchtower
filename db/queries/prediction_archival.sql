-- name: ListPredictionsForArchival :many
-- The v10.3 archival worker pulls terminal predictions that have
-- aged past TerminalRetention. Already-archived rows excluded by
-- the partial index `idx_market_predictions_terminal_for_archival`.
SELECT id, event_slug, condition_id, current_state, updated_at
FROM polymarket_market_predictions
WHERE archived_at IS NULL
  AND current_state IN ('resolved','invalidated','stale','already_priced')
  AND updated_at < @older_than
ORDER BY updated_at ASC
LIMIT @limit_count;

-- name: ArchivePrediction :exec
-- Stamps archived_at + the operator-facing terminal_reason. Idempotent.
UPDATE polymarket_market_predictions
SET archived_at = NOW(),
    terminal_reason = @terminal_reason,
    updated_at  = NOW()
WHERE id = @id
  AND archived_at IS NULL;

-- name: MarkPredictionStaleNoSignal :exec
-- Used by the archival worker's "no fresh signal" gate. Sets state
-- to 'stale' on rows that had no annotation / catalyst update /
-- material price move within StaleNoSignalAfter.
UPDATE polymarket_market_predictions
SET current_state = 'stale',
    state_reason  = @reason,
    updated_at    = NOW()
WHERE id = @id
  AND current_state NOT IN ('resolved','invalidated','stale')
  AND archived_at IS NULL;

-- name: ListPredictionsForStaleSignal :many
-- Selection for the stale-no-signal sweep. The worker filters
-- further (annotations, catalysts) before flipping state.
SELECT id, event_slug, condition_id, current_state, updated_at, last_evolved_at
FROM polymarket_market_predictions
WHERE archived_at IS NULL
  AND current_state NOT IN ('resolved','invalidated','stale')
  AND updated_at < @older_than
ORDER BY updated_at ASC
LIMIT @limit_count;

-- name: UpsertPredictionEvaluation :exec
-- v10.3 evaluation classifier output. One row per (prediction, horizon).
INSERT INTO polymarket_prediction_evaluations
  (prediction_id, horizon, evaluation, score, evidence_json, created_at, updated_at)
VALUES (@prediction_id, @horizon, @evaluation, @score, @evidence_json, NOW(), NOW())
ON CONFLICT (prediction_id, horizon) DO UPDATE
   SET evaluation     = EXCLUDED.evaluation,
       score          = EXCLUDED.score,
       evidence_json  = EXCLUDED.evidence_json,
       updated_at     = NOW();

-- name: ListPredictionEvaluationsForCalibration :many
-- Powers the daily calibration report + CLI. Pulls evaluations
-- newer than `since`, joined with the prediction row so the
-- aggregator sees side_bias, current_state, confidence in one go.
SELECT e.prediction_id, e.horizon, e.evaluation, e.score, e.created_at,
       p.event_slug, p.side_bias, p.current_state, p.confidence
FROM polymarket_prediction_evaluations e
JOIN polymarket_market_predictions p ON p.id = e.prediction_id
WHERE e.created_at >= @since
ORDER BY e.created_at DESC
LIMIT @limit_count;
