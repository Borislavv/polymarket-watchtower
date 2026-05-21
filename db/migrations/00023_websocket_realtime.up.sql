-- 00023_websocket_realtime.up.sql
--
-- v10.4 Hybrid WebSocket Realtime Ingestion. Adds:
--
--  * polymarket_ws_events: raw fast-lane events (book / price_change /
--    last_trade_price / best_bid_ask / tick_size_change / market_resolved /
--    new_market / heartbeat / unknown). NOT authoritative — the
--    polling / data-api / on-chain trade history is still the source
--    of truth. WS rows exist so the realtime worker can correlate
--    triggers against persisted state.
--
--  * polymarket_live_market_state: latest top-of-book / mid /
--    last_price per condition_id. Updated from BOTH WS events AND
--    the reconciliation sweep. One row per condition_id.
--
--  * polymarket_ws_gap_recoveries: audit log of each
--    reconciliation/gap-recovery run (start, end, lookback window,
--    recovered_trades count, status, last_error).
--
--  * polymarket_realtime_work_queue: idempotent queue the WS
--    handler enqueues into. Existing DB-backed workers (repricing,
--    prediction-evolution, etc.) drain it; the WS path never
--    triggers Telegram / AI directly.
--
-- All four tables fail-open: the alert path is independent and
-- never reads from them.

CREATE TABLE IF NOT EXISTS polymarket_ws_events (
    id                  BIGSERIAL    PRIMARY KEY,
    received_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    exchange_timestamp  TIMESTAMPTZ,
    event_type          TEXT         NOT NULL,
    event_slug          TEXT,
    condition_id        TEXT,
    market_slug         TEXT,
    clob_token_id       TEXT,
    outcome             TEXT,
    price               DOUBLE PRECISION,
    size                DOUBLE PRECISION,
    side                TEXT,
    side_source         TEXT         NOT NULL DEFAULT 'unknown',
    side_confidence     DOUBLE PRECISION NOT NULL DEFAULT 0,
    best_bid            DOUBLE PRECISION,
    best_ask            DOUBLE PRECISION,
    mid                 DOUBLE PRECISION,
    tx_hash             TEXT,
    trade_id            TEXT,
    wallet              TEXT,
    sequence            TEXT,
    raw_json            JSONB,
    raw_hash            TEXT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (side_source IN ('websocket','data_api','onchain','inferred','unknown'))
);

CREATE INDEX IF NOT EXISTS idx_ws_events_condition_received
    ON polymarket_ws_events (condition_id, received_at DESC) WHERE condition_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ws_events_event_received
    ON polymarket_ws_events (event_slug, received_at DESC) WHERE event_slug IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ws_events_token_received
    ON polymarket_ws_events (clob_token_id, received_at DESC) WHERE clob_token_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ws_events_type_received
    ON polymarket_ws_events (event_type, received_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_live_market_state (
    condition_id        TEXT         PRIMARY KEY,
    event_slug          TEXT,
    market_slug         TEXT,
    best_bid            DOUBLE PRECISION,
    best_ask            DOUBLE PRECISION,
    mid                 DOUBLE PRECISION,
    last_price          DOUBLE PRECISION,
    last_trade_at       TIMESTAMPTZ,
    last_ws_event_at    TIMESTAMPTZ,
    ws_connected        BOOLEAN      NOT NULL DEFAULT FALSE,
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_live_market_state_event
    ON polymarket_live_market_state (event_slug);

CREATE TABLE IF NOT EXISTS polymarket_ws_gap_recoveries (
    id                  BIGSERIAL    PRIMARY KEY,
    condition_id        TEXT         NOT NULL,
    started_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    ended_at            TIMESTAMPTZ,
    lookback_start      TIMESTAMPTZ,
    lookback_end        TIMESTAMPTZ,
    recovered_trades    INTEGER      NOT NULL DEFAULT 0,
    status              TEXT         NOT NULL DEFAULT 'started',
    last_error          TEXT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (status IN ('started','ok','no_trades','partial','failed'))
);

CREATE INDEX IF NOT EXISTS idx_gap_recoveries_condition
    ON polymarket_ws_gap_recoveries (condition_id, started_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_realtime_work_queue (
    id                  BIGSERIAL    PRIMARY KEY,
    condition_id        TEXT,
    event_slug          TEXT,
    reason              TEXT         NOT NULL,
    priority            SMALLINT     NOT NULL DEFAULT 5,
    dedupe_key          TEXT         NOT NULL,
    available_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    claimed_at          TIMESTAMPTZ,
    attempts            INTEGER      NOT NULL DEFAULT 0,
    last_error          TEXT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_realtime_work_queue_dedup UNIQUE (dedupe_key),
    CHECK (reason IN ('price_move','book_change','trade_seen','market_status','gap_recovered'))
);

-- Hot-path queue claim: rows that are due + not yet claimed.
CREATE INDEX IF NOT EXISTS idx_realtime_work_queue_pending
    ON polymarket_realtime_work_queue (available_at, priority)
    WHERE claimed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_realtime_work_queue_condition
    ON polymarket_realtime_work_queue (condition_id, created_at DESC) WHERE condition_id IS NOT NULL;
