-- 00021_prediction_intelligence.up.sql
--
-- v10.2 Production Intelligence Hardening:
--
--  * polymarket_prediction_usefulness_scores: deterministic per-
--    prediction score (0..1) + components map + reason. Computed on
--    every evolution / creation cycle; one row per prediction with
--    last value live (history is the audit log).
--  * polymarket_prediction_feedback: post-prediction calibration.
--    One row per (prediction_id, horizon) where horizon ∈ (1h, 6h,
--    24h). Records the price at prediction time vs. price at the
--    horizon and whether the predicted direction moved correctly.
--
-- Neither table is on the alert hot path; failure to write a row
-- NEVER blocks alert delivery.

CREATE TABLE IF NOT EXISTS polymarket_prediction_usefulness_scores (
    id              BIGSERIAL    PRIMARY KEY,
    prediction_id   BIGINT       NOT NULL REFERENCES polymarket_market_predictions(id) ON DELETE CASCADE,
    score           DOUBLE PRECISION NOT NULL CHECK (score >= 0 AND score <= 1),
    components_json JSONB,
    reason          TEXT         NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_prediction_usefulness_scores_prediction UNIQUE (prediction_id)
);

CREATE INDEX IF NOT EXISTS idx_prediction_usefulness_scores_score
    ON polymarket_prediction_usefulness_scores (score DESC, updated_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_prediction_feedback (
    id                          BIGSERIAL    PRIMARY KEY,
    prediction_id               BIGINT       NOT NULL REFERENCES polymarket_market_predictions(id) ON DELETE CASCADE,
    horizon                     TEXT         NOT NULL CHECK (horizon IN ('1h','6h','24h')),
    price_at_prediction         DOUBLE PRECISION,
    price_at_horizon            DOUBLE PRECISION,
    price_delta                 DOUBLE PRECISION,
    direction_correct           BOOLEAN, -- NULL when undecidable (missing price)
    state_at_horizon            TEXT,
    repricing_status_at_horizon TEXT,
    catalyst_status_at_horizon  TEXT,
    flow_confirmed              BOOLEAN  NOT NULL DEFAULT FALSE,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_prediction_feedback_horizon UNIQUE (prediction_id, horizon)
);

CREATE INDEX IF NOT EXISTS idx_prediction_feedback_pred
    ON polymarket_prediction_feedback (prediction_id, horizon);

-- The feedback worker periodically asks "which predictions have a
-- pending horizon eligible for measurement?". The partial index
-- powers that selection query.
CREATE INDEX IF NOT EXISTS idx_predictions_for_feedback
    ON polymarket_market_predictions (created_at)
    WHERE current_state NOT IN ('resolved','invalidated');
