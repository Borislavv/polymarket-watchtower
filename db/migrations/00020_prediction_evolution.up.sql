-- 00020_prediction_evolution.up.sql
--
-- v9.9 Prediction Evolution Worker support.
--
-- Adds the per-prediction "last evolution attempted" timestamp so
-- the selection query can filter "predictions whose refresh interval
-- elapsed" without confusing every NOW()-bumping upsert. The column
-- defaults to NULL — a freshly-inserted prediction is immediately
-- eligible for the first evolution cycle.
--
-- The accompanying index supports the priority-ordered selection
-- query ListPredictionsForEvolution.

ALTER TABLE polymarket_market_predictions
    ADD COLUMN IF NOT EXISTS last_evolved_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_market_predictions_evolution_queue
    ON polymarket_market_predictions (last_evolved_at NULLS FIRST, current_state)
    WHERE current_state NOT IN ('resolved', 'invalidated');
