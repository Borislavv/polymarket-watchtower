-- v10.9 Unified Prediction Intelligence Engine — persistence pass.
--
-- Six new tables:
--   1. polymarket_market_ai_cache         — per-market AI decision cache
--   2. polymarket_telegram_semantic_dedupe — operator-facing dedupe
--   3. polymarket_unified_intel_runs       — one row per evaluator cycle
--   4. polymarket_unified_intel_decisions  — per-selected-market output
--   5. polymarket_repricing_theses         — deterministic repricing model
--   6. polymarket_market_price_snapshots   — periodic price sampler

-- =========================================================================
-- 1. Market-level AI cache (PART 3)
-- =========================================================================
-- Keyed by (event_slug, condition_id, ai_surface, market_ai_key). When
-- a candidate market comes through the unified worker the same key
-- (same news fingerprint, same catalyst fingerprint, same buckets) is
-- looked up. A hit means we skip the AI call and reuse the prior
-- decision OR suppress entirely (sentinel results).
CREATE TABLE IF NOT EXISTS polymarket_market_ai_cache (
    id                    BIGSERIAL    PRIMARY KEY,
    event_slug            TEXT         NOT NULL,
    condition_id          TEXT         NOT NULL,
    ai_surface            TEXT         NOT NULL,
    market_ai_key         TEXT         NOT NULL,
    news_fingerprint      TEXT         NOT NULL,
    catalyst_fingerprint  TEXT         NOT NULL DEFAULT '',
    repricing_bucket      TEXT         NOT NULL DEFAULT '',
    flow_bucket           TEXT         NOT NULL DEFAULT '',
    price_bucket          TEXT         NOT NULL DEFAULT '',
    ai_status             TEXT         NOT NULL,
    sentinel_code         TEXT         NOT NULL DEFAULT '',
    decision_json         JSONB,
    summary_text          TEXT         NOT NULL DEFAULT '',
    last_ai_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_reused_at        TIMESTAMPTZ,
    reuse_count           INTEGER      NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (ai_surface, market_ai_key)
);
CREATE INDEX IF NOT EXISTS idx_market_ai_cache_event
    ON polymarket_market_ai_cache(event_slug, condition_id);
CREATE INDEX IF NOT EXISTS idx_market_ai_cache_last_ai_at
    ON polymarket_market_ai_cache(last_ai_at DESC);

-- =========================================================================
-- 2. Telegram semantic dedupe (PART 4)
-- =========================================================================
-- Keyed by (surface, dedupe_key). Increments send_count on each hit
-- but the application logic in alerting decides whether to actually
-- send. Holds the last_notional / last_severity so the escalation
-- factor can be evaluated.
CREATE TABLE IF NOT EXISTS polymarket_telegram_semantic_dedupe (
    id                    BIGSERIAL    PRIMARY KEY,
    surface               TEXT         NOT NULL,
    dedupe_key            TEXT         NOT NULL,
    semantic_fingerprint  TEXT         NOT NULL,
    event_slug            TEXT         NOT NULL DEFAULT '',
    condition_id          TEXT         NOT NULL DEFAULT '',
    wallet                TEXT         NOT NULL DEFAULT '',
    last_sent_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    send_count            INTEGER      NOT NULL DEFAULT 1,
    last_notional         DOUBLE PRECISION,
    last_severity         TEXT         NOT NULL DEFAULT '',
    last_reason           TEXT         NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (surface, dedupe_key)
);
CREATE INDEX IF NOT EXISTS idx_telegram_dedupe_event
    ON polymarket_telegram_semantic_dedupe(event_slug, condition_id);
CREATE INDEX IF NOT EXISTS idx_telegram_dedupe_last_sent
    ON polymarket_telegram_semantic_dedupe(last_sent_at DESC);

-- =========================================================================
-- 3. Unified intel runs (PART 13)
-- =========================================================================
CREATE TABLE IF NOT EXISTS polymarket_unified_intel_runs (
    id                    BIGSERIAL    PRIMARY KEY,
    started_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    finished_at           TIMESTAMPTZ,
    status                TEXT         NOT NULL DEFAULT 'started',
    trigger_reason        TEXT         NOT NULL DEFAULT '',
    input_fingerprint     TEXT         NOT NULL DEFAULT '',
    news_changed_count    INTEGER      NOT NULL DEFAULT 0,
    candidates_count      INTEGER      NOT NULL DEFAULT 0,
    selected_count        INTEGER      NOT NULL DEFAULT 0,
    ai_called             BOOLEAN      NOT NULL DEFAULT FALSE,
    ai_status             TEXT         NOT NULL DEFAULT '',
    sentinel_code         TEXT         NOT NULL DEFAULT '',
    ai_cost_usd           DOUBLE PRECISION NOT NULL DEFAULT 0,
    telegram_sent         BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (status IN ('started','ok','skipped','failed'))
);
CREATE INDEX IF NOT EXISTS idx_unified_intel_runs_started_at
    ON polymarket_unified_intel_runs(started_at DESC);

-- =========================================================================
-- 4. Unified intel decisions (PART 13)
-- =========================================================================
CREATE TABLE IF NOT EXISTS polymarket_unified_intel_decisions (
    id                          BIGSERIAL    PRIMARY KEY,
    run_id                      BIGINT       NOT NULL REFERENCES polymarket_unified_intel_runs(id) ON DELETE CASCADE,
    event_slug                  TEXT         NOT NULL,
    condition_id                TEXT         NOT NULL,
    decision                    TEXT         NOT NULL,
    regime                      TEXT         NOT NULL DEFAULT '',
    class                       TEXT         NOT NULL DEFAULT '',
    interest_score              DOUBLE PRECISION NOT NULL DEFAULT 0,
    confidence                  DOUBLE PRECISION NOT NULL DEFAULT 0,
    current_price               DOUBLE PRECISION,
    expected_direction          TEXT         NOT NULL DEFAULT '',
    expected_price_min          DOUBLE PRECISION,
    expected_price_max          DOUBLE PRECISION,
    expected_window             TEXT         NOT NULL DEFAULT '',
    why_market_misprices        TEXT         NOT NULL DEFAULT '',
    what_market_will_understand TEXT         NOT NULL DEFAULT '',
    trigger_condition           TEXT         NOT NULL DEFAULT '',
    invalidates_if              TEXT         NOT NULL DEFAULT '',
    trade_stance                TEXT         NOT NULL DEFAULT '',
    telegram_worthy             BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_unified_intel_decisions_event
    ON polymarket_unified_intel_decisions(event_slug, condition_id);
CREATE INDEX IF NOT EXISTS idx_unified_intel_decisions_created_at
    ON polymarket_unified_intel_decisions(created_at DESC);

-- =========================================================================
-- 5. Repricing theses (PART 9)
-- =========================================================================
-- Deterministic repricing thesis: computed from price history + flow,
-- not AI. The unified worker writes one row per evaluation cycle per
-- candidate market when the deterministic repricing classifier
-- produces a thesis. Source = 'deterministic' today; future AI-
-- enriched theses can use source = 'ai'.
CREATE TABLE IF NOT EXISTS polymarket_repricing_theses (
    id                     BIGSERIAL    PRIMARY KEY,
    run_id                 BIGINT       REFERENCES polymarket_unified_intel_runs(id) ON DELETE SET NULL,
    event_slug             TEXT         NOT NULL,
    condition_id           TEXT         NOT NULL,
    current_price          DOUBLE PRECISION NOT NULL,
    expected_direction     TEXT         NOT NULL,
    expected_price_min     DOUBLE PRECISION,
    expected_price_max     DOUBLE PRECISION,
    expected_window        TEXT         NOT NULL DEFAULT 'unclear',
    trigger_condition      TEXT         NOT NULL DEFAULT '',
    confidence             DOUBLE PRECISION NOT NULL DEFAULT 0,
    reason                 TEXT         NOT NULL DEFAULT '',
    invalidates_if         TEXT         NOT NULL DEFAULT '',
    source                 TEXT         NOT NULL DEFAULT 'deterministic',
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_repricing_theses_event
    ON polymarket_repricing_theses(event_slug, condition_id);
CREATE INDEX IF NOT EXISTS idx_repricing_theses_created_at
    ON polymarket_repricing_theses(created_at DESC);

-- =========================================================================
-- 6. Market price snapshots (PART 8)
-- =========================================================================
-- Periodic price sampler. The v10.4 WebSocket fast-lane already
-- maintains polymarket_live_market_state with the latest book/mid;
-- this table is the COMPACT TIME-SERIES for the deterministic
-- repricing engine (30m / 1h / 6h / 24h windows). One row per active
-- market per sampler tick (default 60s).
CREATE TABLE IF NOT EXISTS polymarket_market_price_snapshots (
    id            BIGSERIAL    PRIMARY KEY,
    condition_id  TEXT         NOT NULL,
    event_slug    TEXT         NOT NULL DEFAULT '',
    market_slug   TEXT         NOT NULL DEFAULT '',
    price         DOUBLE PRECISION,
    best_bid      DOUBLE PRECISION,
    best_ask      DOUBLE PRECISION,
    mid           DOUBLE PRECISION,
    source        TEXT         NOT NULL DEFAULT 'sampler',
    sampled_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_price_snapshots_condition_time
    ON polymarket_market_price_snapshots(condition_id, sampled_at DESC);
CREATE INDEX IF NOT EXISTS idx_price_snapshots_sampled_at
    ON polymarket_market_price_snapshots(sampled_at DESC);
