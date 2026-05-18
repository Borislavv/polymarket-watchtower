-- 00007_alert_reactions.down.sql

BEGIN;

DROP INDEX IF EXISTS idx_alerts_reaction_pending;

ALTER TABLE polymarket_alerts
    DROP CONSTRAINT IF EXISTS polymarket_alerts_reaction_status_valid,
    DROP COLUMN IF EXISTS telegram_reaction_status,
    DROP COLUMN IF EXISTS telegram_reaction_emoji,
    DROP COLUMN IF EXISTS last_reaction_at;

COMMIT;
