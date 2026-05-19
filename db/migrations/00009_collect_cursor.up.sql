-- 00009_collect_cursor.up.sql
--
-- Decouple the collector's sync cursor from backfill ingestion.
--
-- BEFORE this migration the collector read its per-market cursor as
--   SELECT MAX(traded_at) FROM polymarket_trades WHERE market_id = $1
-- which advanced on EVERY insert — collect's own and backfill's
-- equally. The backfill worker walks the upstream Data API
-- newest-first in pages of 500, so its first page on a newly-
-- discovered market contains very recent trades. Those advance
-- MAX(traded_at) to "now", and collect's next tick asks the API for
-- trades since that timestamp + 1s — which returns nothing.
-- detect.Observe is wired only in collect's hot path, so the live
-- tail is invisible to the detector.
--
-- The fix is a dedicated cursor column updated only by the collect
-- path (persist.Sink.PersistTrades). Backfill keeps writing to
-- polymarket_trades for baseline/analytics correctness but never
-- touches this column, so collect always re-fetches from its own
-- last-seen-trade timestamp regardless of what backfill is doing.
--
-- NULL on the column = first-sight market; the collector falls
-- through to its BootstrapLookback. A market that backfilled
-- successfully but never had a collect tick still gets a proper
-- bootstrap walk on the next collect cycle.

BEGIN;

ALTER TABLE polymarket_markets
    ADD COLUMN last_collect_traded_at TIMESTAMPTZ;

COMMENT ON COLUMN polymarket_markets.last_collect_traded_at IS
    'Cursor for the collect loop. MAX(traded_at) of trades the COLLECT path persisted. Backfill never updates this column.';

COMMIT;
