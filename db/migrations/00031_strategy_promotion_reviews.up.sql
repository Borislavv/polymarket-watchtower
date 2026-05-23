-- v11.6 PART 7 — promotion enforcement table. Append-only.
--
-- One row per (strategy_name, strategy_version, reviewed_at). The
-- strategypromotion worker re-evaluates every Interval and writes a
-- fresh row; the bus reads the most recent eligible=true row to
-- decide whether a non-shadow write is allowed.
CREATE TABLE IF NOT EXISTS polymarket_strategy_promotion_reviews (
    id                       BIGSERIAL    PRIMARY KEY,
    strategy_name            TEXT         NOT NULL,
    strategy_version         TEXT         NOT NULL,
    sample_size              INTEGER      NOT NULL DEFAULT 0,
    median_signed_move_6h    DOUBLE PRECISION NOT NULL DEFAULT 0,
    reversal_15m_ratio       DOUBLE PRECISION NOT NULL DEFAULT 0,
    alerts_per_day           DOUBLE PRECISION NOT NULL DEFAULT 0,
    eligible                 BOOLEAN      NOT NULL DEFAULT FALSE,
    reasons_json             JSONB,
    reviewed_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_strategy_promotion_reviews_latest
    ON polymarket_strategy_promotion_reviews (strategy_name, strategy_version, reviewed_at DESC);
CREATE INDEX IF NOT EXISTS idx_strategy_promotion_reviews_eligible
    ON polymarket_strategy_promotion_reviews (strategy_name, eligible, reviewed_at DESC)
    WHERE eligible IS TRUE;
