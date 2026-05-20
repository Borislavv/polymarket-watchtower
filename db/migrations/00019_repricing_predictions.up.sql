-- 00019_repricing_predictions.up.sql
--
-- v9.8 Intelligence Hardening — three intel-layer tables landing
-- together because their writers and readers are interleaved:
--
--   * polymarket_repricing_signals — one row per
--     (event_slug, condition_id, annotation_hash). Deterministic
--     features computed by internal/app/usecase/repricing.Provider.
--     Drives the "Repricing intelligence" prompt block + powers
--     prediction state transitions.
--
--   * polymarket_market_predictions — evolving prediction state for
--     each (event_slug, condition_id) pair. The state machine in
--     internal/app/usecase/marketprediction owns transitions; this
--     table is the persisted "what we currently think" snapshot.
--
--   * polymarket_market_prediction_states — append-only audit log
--     of every transition. The (prediction_id, created_at) pair is
--     ordered; reading the tail gives the transition history.
--
-- AI / Polymarket-authored text in these tables is DATA. Renderers
-- HTML-escape at the boundary.

CREATE TABLE IF NOT EXISTS polymarket_repricing_signals (
    id                          BIGSERIAL    PRIMARY KEY,
    event_slug                  TEXT         NOT NULL,
    condition_id                TEXT         NOT NULL,
    outcome                     TEXT         NOT NULL DEFAULT '',
    annotation_hash             TEXT         NOT NULL,
    annotation_time             TIMESTAMPTZ,
    annotation_title            TEXT         NOT NULL DEFAULT '',
    price_before                DOUBLE PRECISION,
    price_after                 DOUBLE PRECISION,
    annotation_price_change     DOUBLE PRECISION,
    current_price               DOUBLE PRECISION,
    current_vs_price_after      DOUBLE PRECISION NOT NULL DEFAULT 0,
    drift_since_annotation      DOUBLE PRECISION NOT NULL DEFAULT 0,
    pre_annotation_flow_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
    post_annotation_flow_usd    DOUBLE PRECISION NOT NULL DEFAULT 0,
    same_side_post_flow_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
    opposite_side_post_flow_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    flow_timing                 TEXT         NOT NULL DEFAULT 'unknown'
        CHECK (flow_timing IN ('pre_event_positioning','post_event_chasing','mixed','no_flow','unknown')),
    repricing_status            TEXT         NOT NULL DEFAULT 'unclear'
        CHECK (repricing_status IN ('underreacting','overreacting','already_priced','still_repricing','reversed','unclear')),
    confidence                  DOUBLE PRECISION NOT NULL DEFAULT 0,
    explanation                 TEXT         NOT NULL DEFAULT '',
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_repricing_signal_dedup UNIQUE (event_slug, condition_id, annotation_hash)
);

CREATE INDEX IF NOT EXISTS idx_repricing_signals_event
    ON polymarket_repricing_signals (event_slug, annotation_time DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS idx_repricing_signals_status
    ON polymarket_repricing_signals (repricing_status, created_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_market_predictions (
    id                              BIGSERIAL    PRIMARY KEY,
    event_slug                      TEXT         NOT NULL,
    condition_id                    TEXT         NOT NULL,
    outcome                         TEXT         NOT NULL DEFAULT '',
    side_bias                       TEXT         NOT NULL DEFAULT '',
    summary                         TEXT         NOT NULL DEFAULT '',
    current_state                   TEXT         NOT NULL DEFAULT 'new'
        CHECK (current_state IN (
            'new','watching','blocked','active_catalyst','confirmed_by_flow',
            'contradicted_by_flow','repricing','already_priced','stale',
            'resolved','invalidated'
        )),
    state_reason                    TEXT         NOT NULL DEFAULT '',
    previous_prediction_id          BIGINT,
    supersedes_prediction_id        BIGINT,
    last_repriced_at                TIMESTAMPTZ,
    last_confirmed_by_alert_at      TIMESTAMPTZ,
    last_contradicted_by_alert_at   TIMESTAMPTZ,
    confidence                      DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at                      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at                      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_market_predictions_event_condition UNIQUE (event_slug, condition_id)
);

CREATE INDEX IF NOT EXISTS idx_market_predictions_state
    ON polymarket_market_predictions (current_state, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_market_predictions_event
    ON polymarket_market_predictions (event_slug, updated_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_market_prediction_states (
    id              BIGSERIAL    PRIMARY KEY,
    prediction_id   BIGINT       NOT NULL REFERENCES polymarket_market_predictions(id) ON DELETE CASCADE,
    previous_state  TEXT         NOT NULL DEFAULT '',
    new_state       TEXT         NOT NULL,
    reason          TEXT         NOT NULL DEFAULT '',
    evidence_json   JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_market_prediction_states_prediction
    ON polymarket_market_prediction_states (prediction_id, created_at DESC);
