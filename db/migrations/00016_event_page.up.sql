-- 00016_event_page.up.sql
--
-- Polymarket event-page narrative tables.
--
-- Source: https://polymarket.com/_next/data/<buildId>/en/event/<slug>.json
-- This is the hydrated Next.js payload that backs the Polymarket UI
-- event page. We persist three artifacts per fetch:
--
--   * polymarket_event_page_snapshots — one row per fetch, audit only;
--     raw payload capped to 1 MB by the writer.
--   * polymarket_event_page_markets   — denormalised per-market view
--     used by the lag detector + AI context renderer; one row per
--     (snapshot, market_id).
--   * polymarket_event_annotations    — the market-moving timeline
--     items shown around the event chart; deduped by
--     (event_slug, item_hash) where item_hash is a SHA-256 over
--     event_slug | unix_time | outcome | title (timestamp is
--     deliberately excluded to survive Polymarket back-filling
--     timestamps on the same logical item).
--
-- All three tables are write-tolerant: a fetch failure NEVER blocks
-- the alert path. Empty/zero rows are not inserted; the upsert path
-- short-circuits when the payload omits a section.

CREATE TABLE IF NOT EXISTS polymarket_event_page_snapshots (
    id          BIGSERIAL    PRIMARY KEY,
    event_slug  TEXT         NOT NULL,
    build_id    TEXT         NOT NULL,
    fetched_at  TIMESTAMPTZ  NOT NULL,
    raw_hash    TEXT         NOT NULL,
    raw_json    JSONB,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_event_page_snapshots_event_fetched
    ON polymarket_event_page_snapshots (event_slug, fetched_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_event_page_markets (
    id                       BIGSERIAL    PRIMARY KEY,
    snapshot_id              BIGINT       NOT NULL REFERENCES polymarket_event_page_snapshots(id) ON DELETE CASCADE,
    event_slug               TEXT         NOT NULL,
    market_id                TEXT         NOT NULL,
    condition_id             TEXT         NOT NULL,
    market_slug              TEXT         NOT NULL,
    question                 TEXT         NOT NULL,
    group_item_title         TEXT,
    outcomes_json            JSONB,
    outcome_prices_json      JSONB,
    volume                   DOUBLE PRECISION NOT NULL DEFAULT 0,
    volume_24h               DOUBLE PRECISION NOT NULL DEFAULT 0,
    liquidity                DOUBLE PRECISION NOT NULL DEFAULT 0,
    active                   BOOLEAN      NOT NULL DEFAULT TRUE,
    closed                   BOOLEAN      NOT NULL DEFAULT FALSE,
    end_date                 TIMESTAMPTZ,
    one_hour_price_change    DOUBLE PRECISION,
    one_day_price_change     DOUBLE PRECISION,
    one_week_price_change    DOUBLE PRECISION,
    last_trade_price         DOUBLE PRECISION,
    best_bid                 DOUBLE PRECISION,
    best_ask                 DOUBLE PRECISION,
    clob_token_ids_json      JSONB,
    raw_json                 JSONB,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_event_page_markets_event
    ON polymarket_event_page_markets (event_slug, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_event_page_markets_condition
    ON polymarket_event_page_markets (condition_id);

CREATE TABLE IF NOT EXISTS polymarket_event_annotations (
    id              BIGSERIAL    PRIMARY KEY,
    event_slug      TEXT         NOT NULL,
    item_hash       TEXT         NOT NULL,
    timestamp       TIMESTAMPTZ,
    unix_time       BIGINT       NOT NULL DEFAULT 0,
    time_range      TEXT,
    title           TEXT         NOT NULL,
    summary         TEXT,
    outcome         TEXT,
    price_before    DOUBLE PRECISION,
    price_after     DOUBLE PRECISION,
    price_change    DOUBLE PRECISION,
    source          TEXT,
    sources_json    JSONB,
    tweets_json     JSONB,
    raw_json        JSONB,
    first_seen_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_event_annotations_dedup UNIQUE (event_slug, item_hash)
);

CREATE INDEX IF NOT EXISTS idx_event_annotations_event_time
    ON polymarket_event_annotations (event_slug, timestamp DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS idx_event_annotations_event_outcome_time
    ON polymarket_event_annotations (event_slug, outcome, timestamp DESC NULLS LAST);

-- Fetch state — one row per (event_slug) that records the last
-- attempt + outcome. Used by the refresh policy to decide whether
-- to hit the network again.
CREATE TABLE IF NOT EXISTS polymarket_event_page_fetches (
    event_slug        TEXT         PRIMARY KEY,
    last_fetched_at   TIMESTAMPTZ  NOT NULL,
    last_success_at   TIMESTAMPTZ,
    last_error        TEXT,
    last_build_id     TEXT,
    last_annotations  INTEGER      NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
