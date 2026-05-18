-- 00002_alerts_sending_state.up.sql
--
-- Introduce a transient `sending` status for polymarket_alerts so the
-- AlertSender worker can atomically claim a batch of pending rows without
-- relying on a long-lived transaction across the Telegram round-trip.
--
-- Why this matters: a plain `SELECT … FOR UPDATE SKIP LOCKED` issued via
-- pgx's connection-per-query pool releases the row-lock the moment the
-- query returns, so two concurrent senders both read the same "pending"
-- batch and double-send. The fix is the standard queue-table pattern: an
-- UPDATE … IN (SELECT … FOR UPDATE SKIP LOCKED) RETURNING * that
-- atomically flips status to `sending`. The row is then invisible to a
-- second claimer until MarkSent / MarkFailed advances it.
--
-- A new query (ResetStaleSendingAlerts) will reset `sending` rows older
-- than a configured cutoff back to `pending` so a crashed sender doesn't
-- leave an alert wedged forever.

BEGIN;

ALTER TABLE polymarket_alerts DROP CONSTRAINT polymarket_alerts_status_valid;
ALTER TABLE polymarket_alerts ADD CONSTRAINT polymarket_alerts_status_valid
    CHECK (status IN ('pending','sending','sent','failed'));

-- The partial index used by the claimer must continue to cover the
-- pending-status path only; sending rows are short-lived and read by id.
-- (The existing idx_alerts_status_created already says WHERE status =
-- 'pending'; nothing to change here.)

COMMIT;
