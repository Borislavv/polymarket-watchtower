-- 00006_ownership_concentration.up.sql
--
-- Strategy E (market-ownership concentration). Adds the new alert kind
-- to the CHECK constraint on polymarket_alerts.kind so the
-- alertsender / alerts repo can persist rows for it.
--
-- The detector reads share-flow totals through the new OwnershipShares
-- query (db/queries/trades.sql) — no schema change is needed there
-- beyond the index that idx_trades_market_outcome_time already
-- provides.

BEGIN;

ALTER TABLE polymarket_alerts DROP CONSTRAINT polymarket_alerts_kind_valid;
ALTER TABLE polymarket_alerts ADD CONSTRAINT polymarket_alerts_kind_valid
    CHECK (kind IN ('trade_anomaly','category_watch','accumulation','ownership_concentration'));

COMMIT;
