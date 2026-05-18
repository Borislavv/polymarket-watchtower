-- 00001_init.up.sql
--
-- Phase-2 base schema. See doc/persistence.md for the design rationale.
-- Every column has a single purpose; every index matches a query in
-- db/queries/*.sql. Add a query, add an index — never the other way round.

BEGIN;

-- ---------------------------------------------------------------------------
-- Categories (Polymarket "tags"). One row per tag; `enabled` is the local
-- watchtower view (CATEGORY_WHITELIST), `active` is what Polymarket last
-- returned. A tag that becomes enabled=true but then disappears from Gamma
-- keeps `active=false` and is no longer scheduled for discovery.
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_categories (
    id          BIGSERIAL PRIMARY KEY,
    external_id TEXT        NOT NULL UNIQUE,                 -- gamma tag id, stored as text
    slug        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT FALSE,
    active      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_categories_enabled ON polymarket_categories(enabled) WHERE enabled;

-- ---------------------------------------------------------------------------
-- Markets. One row per Polymarket condition_id. Backfill state lives here
-- (not a separate table) — it's strictly 1:1 with the market and the
-- transitions are tracked via `updated_at` rather than a per-attempt history.
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_markets (
    id                          BIGSERIAL PRIMARY KEY,
    condition_id                TEXT        NOT NULL UNIQUE,
    slug                        TEXT        NOT NULL,
    question                    TEXT        NOT NULL,
    event_slug                  TEXT,
    event_title                 TEXT,
    start_date                  TIMESTAMPTZ,
    end_date                    TIMESTAMPTZ,
    active                      BOOLEAN     NOT NULL DEFAULT TRUE,
    closed                      BOOLEAN     NOT NULL DEFAULT FALSE,
    last_seen_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Backfill state: pending | running | completed | partial_api_limit | failed | skipped
    backfill_status             TEXT        NOT NULL DEFAULT 'pending',
    backfill_oldest_fetched_at  TIMESTAMPTZ,
    backfill_newest_fetched_at  TIMESTAMPTZ,
    backfill_attempts           INT         NOT NULL DEFAULT 0,
    backfill_last_error         TEXT,
    backfill_started_at         TIMESTAMPTZ,
    backfill_completed_at       TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT polymarket_markets_backfill_status_valid CHECK (
        backfill_status IN ('pending','running','completed','partial_api_limit','failed','skipped')
    )
);
CREATE INDEX idx_markets_active_backfill
    ON polymarket_markets(active, backfill_status)
    WHERE active;
CREATE INDEX idx_markets_end_date
    ON polymarket_markets(end_date)
    WHERE active AND end_date IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Market ↔ Category (M:N). Polymarket markets can carry several tags.
-- ON DELETE CASCADE on both sides — orphaned link rows have no meaning.
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_market_categories (
    market_id   BIGINT NOT NULL REFERENCES polymarket_markets(id)    ON DELETE CASCADE,
    category_id BIGINT NOT NULL REFERENCES polymarket_categories(id) ON DELETE CASCADE,
    PRIMARY KEY (market_id, category_id)
);
CREATE INDEX idx_market_categories_category ON polymarket_market_categories(category_id);

-- ---------------------------------------------------------------------------
-- Market outcomes. Each market has 1..N outcome tokens (typically Yes/No).
-- The token_id is the CLOB token id (the same id that appears on trades).
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_market_outcomes (
    id        BIGSERIAL PRIMARY KEY,
    market_id BIGINT NOT NULL REFERENCES polymarket_markets(id) ON DELETE CASCADE,
    token_id  TEXT   NOT NULL,
    label     TEXT   NOT NULL,
    UNIQUE (market_id, token_id)
);

-- ---------------------------------------------------------------------------
-- Traders. One row per wallet address. We collect this lazily as trades are
-- ingested — there is no "discover all traders" endpoint upstream.
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_traders (
    id             BIGSERIAL PRIMARY KEY,
    wallet_address TEXT        NOT NULL UNIQUE,
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- Trades. The single source of truth for every alerting decision.
--
-- Dedup: `dedup_key` is UNIQUE. Callers compute it as the external id when
-- the Data API returns one (the `id` field is a content hash on
-- Polymarket's side, so it survives re-ingest), else a stable SHA-256 over
-- the composite (market_id|outcome|wallet|traded_at|price|size|side).
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_trades (
    id            BIGSERIAL PRIMARY KEY,
    market_id     BIGINT           NOT NULL REFERENCES polymarket_markets(id),
    trader_id     BIGINT           REFERENCES polymarket_traders(id),
    outcome_token TEXT             NOT NULL,
    side          TEXT             NOT NULL,
    price         DOUBLE PRECISION NOT NULL,
    size_shares   DOUBLE PRECISION NOT NULL,
    notional_usd  DOUBLE PRECISION NOT NULL,
    traded_at     TIMESTAMPTZ      NOT NULL,
    external_id   TEXT,
    tx_hash       TEXT,
    ingested_at   TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    dedup_key     TEXT             NOT NULL UNIQUE,

    CONSTRAINT polymarket_trades_side_valid  CHECK (side IN ('BUY','SELL')),
    CONSTRAINT polymarket_trades_price_range CHECK (price > 0 AND price < 1),
    CONSTRAINT polymarket_trades_positive    CHECK (size_shares > 0 AND notional_usd >= 0)
);
CREATE INDEX idx_trades_market_outcome_time
    ON polymarket_trades(market_id, outcome_token, traded_at DESC);
CREATE INDEX idx_trades_market_time
    ON polymarket_trades(market_id, traded_at DESC);
CREATE INDEX idx_trades_trader_time
    ON polymarket_trades(trader_id, traded_at DESC)
    WHERE trader_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Alerts. Insert-on-conflict-do-nothing is the dedup primitive. The sender
-- worker selects `WHERE status='pending'` with FOR UPDATE SKIP LOCKED so
-- concurrent senders never double-send.
--
-- `strategy_version` is also embedded in `dedup_key` ("single:v1:…") but
-- kept as a column too for cheap `WHERE strategy_version=$1` filtering when
-- operators retune thresholds and want to inspect historical decisions.
-- ---------------------------------------------------------------------------
CREATE TABLE polymarket_alerts (
    id                  BIGSERIAL PRIMARY KEY,
    dedup_key           TEXT        NOT NULL UNIQUE,
    strategy_version    TEXT        NOT NULL,
    kind                TEXT        NOT NULL,
    reason              TEXT        NOT NULL,
    severity            TEXT        NOT NULL,
    market_id           BIGINT      REFERENCES polymarket_markets(id),
    trader_id           BIGINT      REFERENCES polymarket_traders(id),
    trade_id            BIGINT      REFERENCES polymarket_trades(id),
    payload             JSONB       NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'pending',
    telegram_message_id BIGINT,
    send_attempts       INT         NOT NULL DEFAULT 0,
    last_send_error     TEXT,
    sent_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT polymarket_alerts_kind_valid     CHECK (kind IN ('trade_anomaly','category_watch')),
    CONSTRAINT polymarket_alerts_status_valid   CHECK (status IN ('pending','sent','failed'))
);
CREATE INDEX idx_alerts_status_created
    ON polymarket_alerts(status, created_at)
    WHERE status = 'pending';
CREATE INDEX idx_alerts_market_created
    ON polymarket_alerts(market_id, created_at DESC)
    WHERE market_id IS NOT NULL;

COMMIT;
