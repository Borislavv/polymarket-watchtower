-- v11.5 Strategy Learning Loop (Wave P0) — seven new tables backing
-- the shadow-first detection layer described in
-- doc/strategy-research/polymarket-politics-strategy-candidates.md
-- and the implementation TЗ.
--
-- Append-only contract. Every table is independently rollback-able
-- via the .down.sql; no existing tables are touched.

-- =========================================================================
-- 1. polymarket_strategy_shadow_decisions
-- =========================================================================
-- The single audit + value-tracking trail for every new strategy.
-- One row per (strategy, decision). Shadow firings AND live firings
-- write here; the shadow_only flag distinguishes them.
--
-- Later-evaluation columns (clv_*, outcome_status) start NULL and
-- are backfilled by the v11.4 drift + outcomes workers via the
-- linked_alert_dedup_key OR by a dedicated shadow-evaluator if no
-- live alert was emitted.
CREATE TABLE IF NOT EXISTS polymarket_strategy_shadow_decisions (
    id                       BIGSERIAL    PRIMARY KEY,
    strategy_name            TEXT         NOT NULL,
    strategy_version         TEXT         NOT NULL,
    condition_id             TEXT         NOT NULL,
    event_slug               TEXT         NOT NULL DEFAULT '',
    wallet                   TEXT         NOT NULL DEFAULT '',
    cohort_id                TEXT,
    side                     TEXT         NOT NULL DEFAULT '',
    decision_kind            TEXT         NOT NULL,
    decision_level           TEXT         NOT NULL DEFAULT 'none',
    score                    DOUBLE PRECISION NOT NULL DEFAULT 0,
    confidence               DOUBLE PRECISION NOT NULL DEFAULT 0,
    reasons_json             JSONB,
    features_json            JSONB,
    shadow_only              BOOLEAN      NOT NULL DEFAULT TRUE,
    fired_at                 TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    linked_alert_dedup_key   TEXT,
    control_bucket_key       TEXT         NOT NULL DEFAULT '',
    -- value-tracking columns; filled async by drift/outcomes workers
    clv_15m                  DOUBLE PRECISION,
    clv_1h                   DOUBLE PRECISION,
    clv_6h                   DOUBLE PRECISION,
    clv_24h                  DOUBLE PRECISION,
    outcome_status           TEXT,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (decision_kind IN ('standalone','boost','suppress','degrade','tag')),
    CHECK (decision_level IN ('info','warning','critical','hard','none'))
);

CREATE INDEX IF NOT EXISTS idx_shadow_decisions_strategy_fired_at
    ON polymarket_strategy_shadow_decisions (strategy_name, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_decisions_condition_id
    ON polymarket_strategy_shadow_decisions (condition_id, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_decisions_control_bucket
    ON polymarket_strategy_shadow_decisions (control_bucket_key, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_decisions_linked_alert
    ON polymarket_strategy_shadow_decisions (linked_alert_dedup_key)
    WHERE linked_alert_dedup_key IS NOT NULL;

-- =========================================================================
-- 2. polymarket_market_links
-- =========================================================================
-- Bessrochnaya graph of market <-> market connections inside a
-- single political thesis. Built by the marketlinks.Builder worker
-- from Gamma events / series / tags. Versioned by link_version so a
-- future graph rebuild doesn't lose history.
CREATE TABLE IF NOT EXISTS polymarket_market_links (
    id                 BIGSERIAL    PRIMARY KEY,
    src_condition_id   TEXT         NOT NULL,
    dst_condition_id   TEXT         NOT NULL,
    link_type          TEXT         NOT NULL,
    -- direction = "aligned" (same thesis direction) | "opposed"
    -- (mirror outcome) | "unknown" (event graph membership only).
    direction          TEXT         NOT NULL DEFAULT 'unknown',
    confidence         DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    event_slug         TEXT         NOT NULL DEFAULT '',
    series_id          TEXT         NOT NULL DEFAULT '',
    link_version       INTEGER      NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (direction IN ('aligned','opposed','unknown')),
    UNIQUE (src_condition_id, dst_condition_id, link_type, link_version)
);

CREATE INDEX IF NOT EXISTS idx_market_links_src
    ON polymarket_market_links (src_condition_id, link_version DESC);
CREATE INDEX IF NOT EXISTS idx_market_links_event_slug
    ON polymarket_market_links (event_slug);

-- =========================================================================
-- 3. polymarket_holder_snapshots
-- =========================================================================
-- Periodic snapshots of top-K holders / per-wallet positions for a
-- (condition_id, outcome_token). Holdersync.Worker upserts a fresh
-- snapshot per (condition_id, snapshot_at). Retention is operator-
-- managed; a follow-up nightly job drops snapshots older than 180d
-- but keeps the most recent per (condition_id, wallet).
CREATE TABLE IF NOT EXISTS polymarket_holder_snapshots (
    id              BIGSERIAL    PRIMARY KEY,
    condition_id    TEXT         NOT NULL,
    outcome_token   TEXT         NOT NULL DEFAULT '',
    snapshot_at     TIMESTAMPTZ  NOT NULL,
    wallet          TEXT         NOT NULL,
    rank            INTEGER      NOT NULL,
    shares          DOUBLE PRECISION NOT NULL,
    notional_usd    DOUBLE PRECISION NOT NULL DEFAULT 0,
    pct_oi          DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_oi        DOUBLE PRECISION NOT NULL DEFAULT 0,
    raw_json        JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (condition_id, outcome_token, snapshot_at, wallet)
);

CREATE INDEX IF NOT EXISTS idx_holder_snapshots_condition_at
    ON polymarket_holder_snapshots (condition_id, snapshot_at DESC);
CREATE INDEX IF NOT EXISTS idx_holder_snapshots_wallet_at
    ON polymarket_holder_snapshots (wallet, snapshot_at DESC);

-- =========================================================================
-- 4. polymarket_book_feature_bars
-- =========================================================================
-- Aggregated orderbook feature time-series per (condition_id,
-- outcome_token, bar_seconds). Written by the realtime worker's
-- aggregator. 1s / 5s buckets retained 30d; rollups retained 180d.
CREATE TABLE IF NOT EXISTS polymarket_book_feature_bars (
    id                  BIGSERIAL    PRIMARY KEY,
    condition_id        TEXT         NOT NULL,
    outcome_token       TEXT         NOT NULL DEFAULT '',
    bar_seconds         INTEGER      NOT NULL,
    bar_start           TIMESTAMPTZ  NOT NULL,
    best_bid            DOUBLE PRECISION,
    best_ask            DOUBLE PRECISION,
    mid_price           DOUBLE PRECISION,
    bid_depth_top_n     DOUBLE PRECISION,
    ask_depth_top_n     DOUBLE PRECISION,
    spread              DOUBLE PRECISION,
    spread_z            DOUBLE PRECISION,
    bid_depth_delta_pct DOUBLE PRECISION,
    ask_depth_delta_pct DOUBLE PRECISION,
    mid_delta           DOUBLE PRECISION,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (condition_id, outcome_token, bar_seconds, bar_start)
);

CREATE INDEX IF NOT EXISTS idx_book_bars_condition_bar
    ON polymarket_book_feature_bars (condition_id, outcome_token, bar_start DESC);

-- =========================================================================
-- 5. polymarket_wallet_graph_edges
-- =========================================================================
-- Behavioral cohort graph: wallets that repeatedly co-trade the
-- same side within short windows across distinct events. Built by
-- walletgraph.Worker. Funding edges (Phase B) reuse the same table
-- with edge_kind='shared_funding' once an external chain provider
-- is wired.
CREATE TABLE IF NOT EXISTS polymarket_wallet_graph_edges (
    id                 BIGSERIAL    PRIMARY KEY,
    wallet_a           TEXT         NOT NULL,
    wallet_b           TEXT         NOT NULL,
    edge_kind          TEXT         NOT NULL,
    similarity_score   DOUBLE PRECISION NOT NULL DEFAULT 0,
    co_events_count    INTEGER      NOT NULL DEFAULT 0,
    cohort_id          TEXT         NOT NULL DEFAULT '',
    first_seen_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    edge_version       INTEGER      NOT NULL DEFAULT 1,
    CHECK (edge_kind IN ('co_trade','shared_funding','shared_bridge','manual')),
    CHECK (wallet_a <= wallet_b),
    UNIQUE (wallet_a, wallet_b, edge_kind, edge_version)
);

CREATE INDEX IF NOT EXISTS idx_wallet_edges_wallet_a
    ON polymarket_wallet_graph_edges (wallet_a, similarity_score DESC);
CREATE INDEX IF NOT EXISTS idx_wallet_edges_cohort
    ON polymarket_wallet_graph_edges (cohort_id)
    WHERE cohort_id <> '';

-- =========================================================================
-- 6. polymarket_market_risk_scores
-- =========================================================================
-- Resolution ambiguity / dispute-risk scoring. One latest row per
-- (condition_id, score_version). Past versions retained for audit.
CREATE TABLE IF NOT EXISTS polymarket_market_risk_scores (
    id                 BIGSERIAL    PRIMARY KEY,
    condition_id       TEXT         NOT NULL,
    score_version      INTEGER      NOT NULL DEFAULT 1,
    ambiguity_score    DOUBLE PRECISION NOT NULL DEFAULT 0,
    dispute_risk       DOUBLE PRECISION NOT NULL DEFAULT 0,
    reasons_json       JSONB,
    is_active          BOOLEAN      NOT NULL DEFAULT TRUE,
    computed_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (condition_id, score_version)
);

CREATE INDEX IF NOT EXISTS idx_market_risk_active
    ON polymarket_market_risk_scores (condition_id)
    WHERE is_active;

-- =========================================================================
-- 7. polymarket_repricing_windows
-- =========================================================================
-- Open/close windows after annotations or catalysts during which
-- the repricing-lag worker watches for under-/over-reaction. Each
-- window is bounded; the worker writes a shadow_decisions row when
-- it expires, even if no lag was found.
CREATE TABLE IF NOT EXISTS polymarket_repricing_windows (
    id                  BIGSERIAL    PRIMARY KEY,
    condition_id        TEXT         NOT NULL,
    event_slug          TEXT         NOT NULL DEFAULT '',
    trigger_kind        TEXT         NOT NULL,
    trigger_ref         TEXT         NOT NULL DEFAULT '',
    opened_at           TIMESTAMPTZ  NOT NULL,
    closes_at           TIMESTAMPTZ  NOT NULL,
    expected_impact_min DOUBLE PRECISION,
    expected_impact_max DOUBLE PRECISION,
    side_bias           TEXT         NOT NULL DEFAULT '',
    baseline_price      DOUBLE PRECISION,
    status              TEXT         NOT NULL DEFAULT 'open',
    resolved_at         TIMESTAMPTZ,
    observed_move       DOUBLE PRECISION,
    peer_move           DOUBLE PRECISION,
    lag_score           DOUBLE PRECISION,
    notes              TEXT         NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (status IN ('open','closed_no_lag','closed_lag_detected','closed_blocked'))
);

CREATE INDEX IF NOT EXISTS idx_repricing_windows_open
    ON polymarket_repricing_windows (closes_at)
    WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_repricing_windows_condition_at
    ON polymarket_repricing_windows (condition_id, opened_at DESC);
