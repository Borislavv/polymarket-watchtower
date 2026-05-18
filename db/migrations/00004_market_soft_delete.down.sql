-- 00004_market_soft_delete.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_markets_active_backfill;
CREATE INDEX idx_markets_active_backfill
    ON polymarket_markets(active, backfill_status)
    WHERE active;

DROP INDEX IF EXISTS idx_markets_deleted_at;

ALTER TABLE polymarket_markets
    DROP COLUMN IF EXISTS purged_at,
    DROP COLUMN IF EXISTS deleted_at;

COMMIT;
