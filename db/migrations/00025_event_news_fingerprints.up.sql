-- v10.7 News-driven AI gating.
--
-- For every Polymarket event_slug, we maintain a stable fingerprint
-- over the annotation/news set. AI is allowed to run for marketintel
-- and prediction-evolution ONLY when the fingerprint changed since
-- the last AI cycle (or a secondary trigger fires — price move /
-- new alert / catalyst change / repricing change).
--
-- The fingerprint hashes ONLY meaningful, stable fields:
--   - annotation hashes (the `item_hash` we already dedup on)
--   - annotation titles + timestamps + outcome + price-before/after
--   - source names/URLs
-- It does NOT hash:
--   - now()
--   - fetch_time / DB jitter
--   - ordering noise (annotations are sorted before hashing)
--
-- Persistence keeps:
--   first_seen_at / last_seen_at — when we first / last computed it.
--   changed_at — last time the fingerprint flipped.
--   last_seen_unchanged_at — the most recent timestamp we observed
--     the SAME fingerprint (used by the "skip AI when news unchanged"
--     gating).
--
-- Rows are upserted by event_slug; the table is small (one row per
-- monitored event, ~few hundred rows in prod).
CREATE TABLE IF NOT EXISTS polymarket_event_news_fingerprints (
    event_slug              TEXT        NOT NULL PRIMARY KEY,
    news_fingerprint        TEXT        NOT NULL,
    annotation_count        INTEGER     NOT NULL DEFAULT 0,
    latest_annotation_at    TIMESTAMPTZ,
    first_seen_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    changed_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_unchanged_at  TIMESTAMPTZ,
    last_ai_called_at       TIMESTAMPTZ,
    -- Tracks the last semantic-fingerprint output we shipped for
    -- this event (cooldown / dedup key for "no edge" suppression).
    last_semantic_fingerprint TEXT,
    last_semantic_at        TIMESTAMPTZ,
    last_semantic_code      TEXT,
    repeated_count          INTEGER     NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_event_news_fingerprints_changed_at
    ON polymarket_event_news_fingerprints(changed_at DESC);
