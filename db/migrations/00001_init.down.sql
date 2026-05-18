-- 00001_init.down.sql
--
-- Reverses 00001_init.up.sql. Drops are in reverse-FK order. Safe to run
-- against an empty DB (IF EXISTS). The `migrations` table itself is owned
-- by the runner (golang-migrate), not by this file.

BEGIN;

DROP TABLE IF EXISTS polymarket_alerts;
DROP TABLE IF EXISTS polymarket_trades;
DROP TABLE IF EXISTS polymarket_traders;
DROP TABLE IF EXISTS polymarket_market_outcomes;
DROP TABLE IF EXISTS polymarket_market_categories;
DROP TABLE IF EXISTS polymarket_markets;
DROP TABLE IF EXISTS polymarket_categories;

COMMIT;
