DROP INDEX IF EXISTS idx_trades_detection_claimed_at;
DROP INDEX IF EXISTS idx_trades_detection_pending;
CREATE INDEX idx_trades_detection_pending
    ON polymarket_trades (traded_at DESC, id DESC)
    WHERE detected_at IS NULL;
ALTER TABLE polymarket_trades
    DROP COLUMN IF EXISTS detection_worker_id,
    DROP COLUMN IF EXISTS detection_claimed_at;
