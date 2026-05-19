-- 00014_intel_period_key — add a deterministic period bucket so two
-- ticks landing in the same 2h window collapse to a single row.
--
-- Background: the previous dedup primitive was UNIQUE (summary_hash),
-- a SHA-256 over the rendered report body. The body embeds the
-- absolute `now` timestamp in the "period:" line, so every tick
-- produced a unique hash and the dedup never triggered. A restart
-- or fast clock skew between replicas resulted in multiple Telegram
-- sends per intended window — the spam the operator reported.
--
-- Fix: compute the bucket boundary deterministically in the worker
-- (period_end = now.Truncate(interval)) and store period_key formed
-- from the boundary's UTC RFC3339 string. UNIQUE on period_key turns
-- "same period, same content or different content" into "exactly
-- one row, ON CONFLICT DO NOTHING".
--
-- summary_hash is kept as a secondary dedup so a manual replay
-- against the same content (different window) still no-ops; its
-- uniqueness index is dropped because intentional re-fires across
-- periods would now collide. The migration backfills existing rows
-- using their stored period_start so the NOT NULL + UNIQUE flip is
-- safe.

ALTER TABLE polymarket_market_intelligence_reports
    ADD COLUMN period_key TEXT NULL;

UPDATE polymarket_market_intelligence_reports
   SET period_key = to_char(period_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') || '/' ||
                    to_char(period_end   AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
 WHERE period_key IS NULL;

ALTER TABLE polymarket_market_intelligence_reports
    ALTER COLUMN period_key SET NOT NULL;

-- summary_hash stays as a column but its UNIQUE index is replaced by
-- a non-unique index — same-period dedup is now the load-bearing
-- contract. Two intentionally-different periods may legitimately
-- produce the same content (unlikely but possible) and we don't want
-- the second one rejected.
DROP INDEX IF EXISTS idx_intel_reports_summary_hash;
CREATE INDEX idx_intel_reports_summary_hash
    ON polymarket_market_intelligence_reports (summary_hash);

CREATE UNIQUE INDEX idx_intel_reports_period_key
    ON polymarket_market_intelligence_reports (period_key);
