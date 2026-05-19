-- 00013_strategy_attribution — bucketed attribution per alert so SQL/Grafana
-- "which setups actually win?" can be answered without recomputing
-- buckets from raw payloads.
--
-- One row per alert (UNIQUE on alert_id). Buckets are coarse strings
-- so cardinality stays bounded — Grafana panels can group-by these
-- dimensions and join against polymarket_alerts.outcome_status to
-- get win-rate by setup family.

CREATE TABLE polymarket_alert_strategy_dimensions (
    alert_id              BIGINT PRIMARY KEY REFERENCES polymarket_alerts(id) ON DELETE CASCADE,
    strategy_family       TEXT        NOT NULL,
    lifecycle_bucket      TEXT        NOT NULL,
    odds_bucket           TEXT        NULL,
    notional_bucket       TEXT        NULL,
    return_bucket         TEXT        NULL,
    category              TEXT        NULL,
    accumulation_window   TEXT        NULL,
    ownership_share_bucket TEXT       NULL,
    volatility_regime     TEXT        NULL,
    new_wallet            BOOLEAN     NOT NULL DEFAULT FALSE,
    quiet_market          BOOLEAN     NOT NULL DEFAULT FALSE,
    dormant_wallet        BOOLEAN     NOT NULL DEFAULT FALSE,
    drift_regime          TEXT        NULL,
    ai_verdict            TEXT        NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_alert_strategy_dimensions_family
    ON polymarket_alert_strategy_dimensions (strategy_family);
CREATE INDEX idx_alert_strategy_dimensions_category
    ON polymarket_alert_strategy_dimensions (category);
