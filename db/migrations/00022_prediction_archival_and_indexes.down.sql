DROP INDEX IF EXISTS idx_event_page_markets_event_condition;
DROP INDEX IF EXISTS idx_repricing_signals_event_updated;
DROP INDEX IF EXISTS idx_prediction_usefulness_scores_updated;
DROP INDEX IF EXISTS idx_predictions_feedback_eligible;
DROP INDEX IF EXISTS idx_prediction_evaluations_class;
DROP INDEX IF EXISTS idx_prediction_evaluations_pred;
DROP TABLE IF EXISTS polymarket_prediction_evaluations;
DROP INDEX IF EXISTS idx_market_predictions_event_created;
DROP INDEX IF EXISTS idx_market_predictions_terminal_for_archival;
DROP INDEX IF EXISTS idx_market_predictions_evolution_queue;
CREATE INDEX IF NOT EXISTS idx_market_predictions_evolution_queue
    ON polymarket_market_predictions (last_evolved_at NULLS FIRST, current_state)
    WHERE current_state NOT IN ('resolved','invalidated');

ALTER TABLE polymarket_market_predictions DROP COLUMN IF EXISTS terminal_reason;
ALTER TABLE polymarket_market_predictions DROP COLUMN IF EXISTS archived_at;
