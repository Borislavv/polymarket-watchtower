-- 00004_market_soft_delete.up.sql
--
-- Ended-market lifecycle (Strategy v4 cleanup).
--
-- When a market disappears from the latest Polymarket sweep (or arrives
-- with closed=true), discovery stamps `deleted_at = NOW()` as a soft
-- delete marker. The market stays in the DB so:
--   - persisted trades remain queryable for long-term analytics (FK
--     polymarket_trades.market_id → polymarket_markets.id has no CASCADE
--     on the trades side, so dropping the market would break history);
--   - if the market resumes, discover/sanity can clear the marker and
--     restart processing.
--
-- After a retention window (default 30 days), the sanity worker re-checks
-- the upstream state. If the market is still gone, the row is stamped
-- `purged_at = NOW()` and excluded from all active processing — but the
-- row is NOT deleted, so trades are retained.

BEGIN;

ALTER TABLE polymarket_markets
    ADD COLUMN deleted_at TIMESTAMPTZ NULL,
    ADD COLUMN purged_at  TIMESTAMPTZ NULL;

-- Soft-deleted markets eligible for sanity worker review. Partial index
-- because the common-case (live markets) carries deleted_at IS NULL.
CREATE INDEX idx_markets_deleted_at
    ON polymarket_markets(deleted_at)
    WHERE deleted_at IS NOT NULL AND purged_at IS NULL;

-- Drop the old "active partial" index; replace with one that also
-- excludes soft-deleted / purged rows so collect/backfill don't pick
-- them up.
DROP INDEX idx_markets_active_backfill;
CREATE INDEX idx_markets_active_backfill
    ON polymarket_markets(active, backfill_status)
    WHERE active AND deleted_at IS NULL AND purged_at IS NULL;

COMMIT;
