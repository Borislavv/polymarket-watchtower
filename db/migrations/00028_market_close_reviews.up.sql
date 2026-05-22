-- v11.4 Market Close Review — append-only persistence for the
-- post-resolution learning loop.
--
-- One row per (market, attempt). Unique succeeded review per
-- market is enforced by a partial unique index so failed
-- attempts can retry with backoff. The append-only contract is
-- preserved by writing every attempt rather than mutating prior
-- rows (only status / attempts / error / ai_* / verdict /
-- admin_summary fields update on UPSERT).
CREATE TABLE IF NOT EXISTS polymarket_market_close_reviews (
    id                  BIGSERIAL    PRIMARY KEY,
    market_id           BIGINT,
    condition_id        TEXT         NOT NULL,
    event_slug          TEXT         NOT NULL DEFAULT '',
    closed_at           TIMESTAMPTZ  NOT NULL,
    resolved_at         TIMESTAMPTZ,
    reviewed_at         TIMESTAMPTZ,

    status              TEXT         NOT NULL DEFAULT 'pending',
    skip_reason         TEXT         NOT NULL DEFAULT '',
    verdict             TEXT         NOT NULL DEFAULT '',
    confidence          DOUBLE PRECISION,

    admin_summary       TEXT         NOT NULL DEFAULT '',
    ai_json             JSONB,
    evidence_json       JSONB,

    ai_model            TEXT         NOT NULL DEFAULT '',
    input_tokens        INTEGER,
    output_tokens       INTEGER,
    estimated_cost_usd  DOUBLE PRECISION,

    error               TEXT         NOT NULL DEFAULT '',
    attempts            INTEGER      NOT NULL DEFAULT 0,
    next_retry_at       TIMESTAMPTZ,

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CHECK (status IN ('pending','running','succeeded','failed','skipped'))
);

-- Per-market dedup of succeeded reviews. A failed/skipped review
-- can retry; once a succeeded row exists for a condition_id the
-- worker treats it as terminal and never re-runs.
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_close_reviews_succeeded_per_market
    ON polymarket_market_close_reviews (condition_id)
    WHERE status = 'succeeded';

-- Candidate-selection helper: find recently-closed markets that
-- haven't been reviewed yet. The worker scans this index ordered
-- by closed_at DESC.
CREATE INDEX IF NOT EXISTS idx_market_close_reviews_status_closed_at
    ON polymarket_market_close_reviews (status, closed_at DESC);

-- Retry queue: pull rows whose next_retry_at <= NOW().
CREATE INDEX IF NOT EXISTS idx_market_close_reviews_next_retry_at
    ON polymarket_market_close_reviews (next_retry_at)
    WHERE status = 'failed' AND next_retry_at IS NOT NULL;
