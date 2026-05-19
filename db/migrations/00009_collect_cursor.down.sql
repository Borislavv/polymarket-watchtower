-- 00009_collect_cursor.down.sql
BEGIN;
ALTER TABLE polymarket_markets DROP COLUMN IF EXISTS last_collect_traded_at;
COMMIT;
