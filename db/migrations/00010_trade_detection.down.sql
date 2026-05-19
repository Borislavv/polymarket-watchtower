DROP INDEX IF EXISTS idx_trades_detection_pending;
ALTER TABLE polymarket_trades
    DROP CONSTRAINT IF EXISTS polymarket_trades_detection_status_valid,
    DROP COLUMN IF EXISTS last_detection_error,
    DROP COLUMN IF EXISTS detection_attempts,
    DROP COLUMN IF EXISTS detection_skip_reason,
    DROP COLUMN IF EXISTS detection_status,
    DROP COLUMN IF EXISTS detected_at;
