-- 00006_ownership_concentration.down.sql
--
-- Revert the alert-kind CHECK constraint to the pre-Strategy-E set.

BEGIN;

ALTER TABLE polymarket_alerts DROP CONSTRAINT polymarket_alerts_kind_valid;
ALTER TABLE polymarket_alerts ADD CONSTRAINT polymarket_alerts_kind_valid
    CHECK (kind IN ('trade_anomaly','category_watch','accumulation'));

COMMIT;
