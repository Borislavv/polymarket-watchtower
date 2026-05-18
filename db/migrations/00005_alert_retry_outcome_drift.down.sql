-- 00005_alert_retry_outcome_drift.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_alerts_drift_pending;
DROP INDEX IF EXISTS idx_alerts_outcome_pending;
DROP INDEX IF EXISTS idx_alerts_failed_retry;

ALTER TABLE polymarket_alerts
    DROP CONSTRAINT IF EXISTS polymarket_alerts_drift_status_valid,
    DROP CONSTRAINT IF EXISTS polymarket_alerts_outcome_status_valid,
    DROP COLUMN IF EXISTS clv_24h,
    DROP COLUMN IF EXISTS clv_6h,
    DROP COLUMN IF EXISTS clv_1h,
    DROP COLUMN IF EXISTS clv_15m,
    DROP COLUMN IF EXISTS drift_checked_at,
    DROP COLUMN IF EXISTS drift_status,
    DROP COLUMN IF EXISTS winning_outcome_label,
    DROP COLUMN IF EXISTS winning_outcome_token,
    DROP COLUMN IF EXISTS resolved_at,
    DROP COLUMN IF EXISTS outcome_checked_at,
    DROP COLUMN IF EXISTS outcome_status,
    DROP COLUMN IF EXISTS last_attempt_at,
    DROP COLUMN IF EXISTS next_retry_at;

COMMIT;
