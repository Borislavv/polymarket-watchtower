DROP INDEX IF EXISTS idx_alert_analyses_legacy_failure;
ALTER TABLE polymarket_alert_analyses DROP COLUMN IF EXISTS legacy_provider_failure;
DROP INDEX IF EXISTS idx_ai_request_logs_status;
DROP INDEX IF EXISTS idx_ai_request_logs_target;
DROP INDEX IF EXISTS idx_ai_request_logs_created_at;
DROP TABLE IF EXISTS polymarket_ai_request_logs;
