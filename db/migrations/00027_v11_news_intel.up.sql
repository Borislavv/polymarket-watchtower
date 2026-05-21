-- v11.0 Hourly News Intelligence — persistence pass (PART 6).
--
-- The v11.0 product replaces the prediction-evolution + market-intel
-- surfaces with ONE hourly news intelligence cycle. Three tables:
--   1. polymarket_news_intel_runs            — one row per hourly cycle
--   2. polymarket_news_intel_decisions       — per-selected news item output
--   3. polymarket_news_intel_processed_items — item_hash dedupe ledger
--
-- These tables exist alongside the v10.9 unified intel tables but are
-- consumed by an entirely separate worker. The legacy tables are not
-- touched by this migration; they retain historical rows.

-- =========================================================================
-- 1. News intel runs
-- =========================================================================
-- One row per hourly cycle of the news intelligence worker. Captures
-- whether the AI was actually called (it is NOT when zero new items were
-- found), the sentinel returned (if any), input/output fingerprints for
-- audit + dedupe, and the Telegram-send outcome.
CREATE TABLE IF NOT EXISTS polymarket_news_intel_runs (
    id                  BIGSERIAL    PRIMARY KEY,
    started_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    finished_at         TIMESTAMPTZ,
    status              TEXT         NOT NULL DEFAULT 'started',
    lookback_start      TIMESTAMPTZ  NOT NULL,
    lookback_end        TIMESTAMPTZ  NOT NULL,
    news_items_count    INTEGER      NOT NULL DEFAULT 0,
    selected_count      INTEGER      NOT NULL DEFAULT 0,
    ai_called           BOOLEAN      NOT NULL DEFAULT FALSE,
    ai_status           TEXT         NOT NULL DEFAULT '',
    sentinel_code       TEXT         NOT NULL DEFAULT '',
    ai_cost_usd         DOUBLE PRECISION NOT NULL DEFAULT 0,
    input_fingerprint   TEXT         NOT NULL DEFAULT '',
    output_fingerprint  TEXT         NOT NULL DEFAULT '',
    telegram_sent       BOOLEAN      NOT NULL DEFAULT FALSE,
    last_error          TEXT         NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (status IN ('started','ok','skipped','failed'))
);
CREATE INDEX IF NOT EXISTS idx_news_intel_runs_started_at
    ON polymarket_news_intel_runs(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_news_intel_runs_status
    ON polymarket_news_intel_runs(status, started_at DESC);

-- =========================================================================
-- 2. News intel decisions
-- =========================================================================
-- Per-selected news item output from the AI. One news item may touch
-- multiple markets; the AI returns one decision per (news_item, market)
-- pair so we keep the same shape here. affected_markets_json holds the
-- compact list of (condition_id, event_slug, market_title) tuples the
-- worker provided as input — useful for debugging "AI picked condition X
-- but related markets Y, Z were also in scope".
CREATE TABLE IF NOT EXISTS polymarket_news_intel_decisions (
    id                          BIGSERIAL    PRIMARY KEY,
    run_id                      BIGINT       NOT NULL REFERENCES polymarket_news_intel_runs(id) ON DELETE CASCADE,
    news_item_hash              TEXT         NOT NULL,
    event_slug                  TEXT         NOT NULL DEFAULT '',
    condition_id                TEXT         NOT NULL DEFAULT '',
    market_title                TEXT         NOT NULL DEFAULT '',
    rank                        INTEGER      NOT NULL DEFAULT 0,
    decision                    TEXT         NOT NULL DEFAULT '',
    confidence                  DOUBLE PRECISION NOT NULL DEFAULT 0,
    impact_direction            TEXT         NOT NULL DEFAULT '',
    expected_price_impact_min   DOUBLE PRECISION,
    expected_price_impact_max   DOUBLE PRECISION,
    expected_window             TEXT         NOT NULL DEFAULT '',
    why_it_matters              TEXT         NOT NULL DEFAULT '',
    what_market_may_miss        TEXT         NOT NULL DEFAULT '',
    trigger_condition           TEXT         NOT NULL DEFAULT '',
    invalidates_if              TEXT         NOT NULL DEFAULT '',
    trade_stance                TEXT         NOT NULL DEFAULT '',
    telegram_worthy             BOOLEAN      NOT NULL DEFAULT FALSE,
    affected_markets_json       JSONB,
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_news_intel_decisions_run
    ON polymarket_news_intel_decisions(run_id);
CREATE INDEX IF NOT EXISTS idx_news_intel_decisions_event
    ON polymarket_news_intel_decisions(event_slug, condition_id);
CREATE INDEX IF NOT EXISTS idx_news_intel_decisions_news_hash
    ON polymarket_news_intel_decisions(news_item_hash);
CREATE INDEX IF NOT EXISTS idx_news_intel_decisions_created_at
    ON polymarket_news_intel_decisions(created_at DESC);

-- =========================================================================
-- 3. News intel processed items
-- =========================================================================
-- Dedupe ledger. The worker collects candidate news items from the
-- annotation pool, computes a stable item_hash, and looks them up
-- against this table. Items already present are skipped — only NEW
-- items reach the AI. last_run_id + processed_at track which cycle
-- consumed the item.
CREATE TABLE IF NOT EXISTS polymarket_news_intel_processed_items (
    item_hash        TEXT         PRIMARY KEY,
    event_slug       TEXT         NOT NULL DEFAULT '',
    title            TEXT         NOT NULL DEFAULT '',
    first_seen_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    processed_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_run_id      BIGINT       REFERENCES polymarket_news_intel_runs(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_news_intel_processed_event
    ON polymarket_news_intel_processed_items(event_slug);
CREATE INDEX IF NOT EXISTS idx_news_intel_processed_at
    ON polymarket_news_intel_processed_items(processed_at DESC);
