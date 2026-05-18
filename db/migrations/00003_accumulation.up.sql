-- 00003_accumulation.up.sql
--
-- Strategy v4: same-trader accumulation line detection.
--
-- A line is the set of trades from one wallet on one (market, outcome, side)
-- inside the accumulation window. The detector runs this query per-trade on
-- the hot path, so we need an index that supports the exact filter.
--
-- We also extend polymarket_alerts.kind to accept the new 'accumulation'
-- alert kind alongside 'trade_anomaly' and 'category_watch'. Dedup still
-- uses the unique dedup_key column.

BEGIN;

-- Composite index for the accumulation query. Order matches the WHERE
-- clause exactly: (trader_id, market_id, outcome_token, side, traded_at).
-- Conditional on trader_id NOT NULL because trades whose trader has not
-- been persisted yet cannot belong to a wallet-scoped line.
CREATE INDEX idx_trades_trader_market_outcome_side_time
    ON polymarket_trades(trader_id, market_id, outcome_token, side, traded_at DESC)
    WHERE trader_id IS NOT NULL;

-- Relax the alert kind constraint to include 'accumulation'.
ALTER TABLE polymarket_alerts DROP CONSTRAINT polymarket_alerts_kind_valid;
ALTER TABLE polymarket_alerts ADD CONSTRAINT polymarket_alerts_kind_valid
    CHECK (kind IN ('trade_anomaly','category_watch','accumulation'));

COMMIT;
