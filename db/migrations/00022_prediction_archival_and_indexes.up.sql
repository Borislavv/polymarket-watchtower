-- 00022_prediction_archival_and_indexes.up.sql
--
-- v10.3 Production Readiness Hardening:
--
--  * polymarket_market_predictions gains archived_at +
--    terminal_reason for the v10.3 archival worker. The selection
--    queue + active-list filters now ignore archived rows so the
--    UI/dashboard never shows them in the "active" set.
--  * polymarket_prediction_evaluations: deterministic classifier
--    output, one row per (prediction_id, horizon).
--  * Indexes added for v10.3 hot-path queries — these prevent the
--    unbounded scans the audit flagged in PART 6.

ALTER TABLE polymarket_market_predictions
    ADD COLUMN IF NOT EXISTS archived_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS terminal_reason  TEXT;

-- The selection queue already excludes resolved/invalidated rows;
-- v10.3 adds the archived guard too. Partial index keeps the live
-- working set small.
DROP INDEX IF EXISTS idx_market_predictions_evolution_queue;
CREATE INDEX IF NOT EXISTS idx_market_predictions_evolution_queue
    ON polymarket_market_predictions (last_evolved_at NULLS FIRST, current_state)
    WHERE current_state NOT IN ('resolved','invalidated')
      AND archived_at IS NULL;

-- v10.3 archival selection query: terminal rows aging past retention.
CREATE INDEX IF NOT EXISTS idx_market_predictions_terminal_for_archival
    ON polymarket_market_predictions (current_state, updated_at)
    WHERE archived_at IS NULL
      AND current_state IN ('resolved','invalidated','stale','already_priced');

-- Per-event clustering for the creation-dedupe query.
CREATE INDEX IF NOT EXISTS idx_market_predictions_event_created
    ON polymarket_market_predictions (event_slug, created_at DESC);

-- v10.3 evaluation table.
CREATE TABLE IF NOT EXISTS polymarket_prediction_evaluations (
    id              BIGSERIAL    PRIMARY KEY,
    prediction_id   BIGINT       NOT NULL REFERENCES polymarket_market_predictions(id) ON DELETE CASCADE,
    horizon         TEXT         NOT NULL CHECK (horizon IN ('1h','6h','24h','72h')),
    evaluation      TEXT         NOT NULL CHECK (evaluation IN (
        'useful_correct','useful_early','correct_but_late','stale_no_move',
        'wrong_direction','already_priced_noise','blocked_unresolved','insufficient_data'
    )),
    score           DOUBLE PRECISION NOT NULL DEFAULT 0,
    evidence_json   JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_prediction_evaluations_horizon UNIQUE (prediction_id, horizon)
);

CREATE INDEX IF NOT EXISTS idx_prediction_evaluations_pred
    ON polymarket_prediction_evaluations (prediction_id, horizon);

CREATE INDEX IF NOT EXISTS idx_prediction_evaluations_class
    ON polymarket_prediction_evaluations (evaluation, created_at DESC);

-- Feedback worker selection query uses created_at + state.
CREATE INDEX IF NOT EXISTS idx_predictions_feedback_eligible
    ON polymarket_market_predictions (created_at)
    WHERE current_state NOT IN ('resolved','invalidated')
      AND archived_at IS NULL;

-- Usefulness scores: dashboard "top N" + per-prediction read.
CREATE INDEX IF NOT EXISTS idx_prediction_usefulness_scores_updated
    ON polymarket_prediction_usefulness_scores (updated_at DESC);

-- Repricing per-event recency for the AI prompt + dashboards.
CREATE INDEX IF NOT EXISTS idx_repricing_signals_event_updated
    ON polymarket_repricing_signals (event_slug, updated_at DESC);

-- Event-page markets lookup by event_slug+condition_id used by the
-- outcome mapper + repricing pipeline.
CREATE INDEX IF NOT EXISTS idx_event_page_markets_event_condition
    ON polymarket_event_page_markets (event_slug, condition_id);
