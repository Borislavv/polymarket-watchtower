-- 00020_prediction_evolution.down.sql
DROP INDEX IF EXISTS idx_market_predictions_evolution_queue;
ALTER TABLE polymarket_market_predictions DROP COLUMN IF EXISTS last_evolved_at;
