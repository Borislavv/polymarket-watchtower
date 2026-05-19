-- 00011_detection_lease — lease + worker-id columns for the v6
-- detection worker so concurrent + restarted workers never lose
-- (or double-process) rows.
--
-- Why: 00010 added detected_at IS NULL as the pending-claim filter
-- via FOR UPDATE SKIP LOCKED. That works when the worker holds the
-- transaction across the entire processing window. In production
-- the worker handles each row OUTSIDE the claim transaction so the
-- lock doesn't pin a pgxpool connection for the full duration. The
-- claim therefore needs a server-side lease (detection_claimed_at)
-- that recovers crashed-mid-process rows after DETECTION_CLAIM_TTL.

ALTER TABLE polymarket_trades
    ADD COLUMN detection_claimed_at TIMESTAMPTZ NULL,
    ADD COLUMN detection_worker_id  TEXT        NULL;

-- The partial index now also excludes leased rows so claim queries
-- skip the in-flight pile without expensive predicate evaluation.
DROP INDEX IF EXISTS idx_trades_detection_pending;
CREATE INDEX idx_trades_detection_pending
    ON polymarket_trades (traded_at DESC, id DESC)
    WHERE detected_at IS NULL;

-- A second index helps the stale-claim reset query (claimed-too-long
-- rows) without scanning the whole table.
CREATE INDEX IF NOT EXISTS idx_trades_detection_claimed_at
    ON polymarket_trades (detection_claimed_at)
    WHERE detected_at IS NULL AND detection_claimed_at IS NOT NULL;
