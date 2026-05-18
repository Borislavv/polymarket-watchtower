-- 00003_accumulation.down.sql

BEGIN;

ALTER TABLE polymarket_alerts DROP CONSTRAINT polymarket_alerts_kind_valid;
ALTER TABLE polymarket_alerts ADD CONSTRAINT polymarket_alerts_kind_valid
    CHECK (kind IN ('trade_anomaly','category_watch'));

DROP INDEX IF EXISTS idx_trades_trader_market_outcome_side_time;

COMMIT;
