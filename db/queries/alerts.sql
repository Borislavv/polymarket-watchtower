-- name: TryCreatePendingAlert :one
-- Atomic dedup primitive. INSERT ON CONFLICT DO NOTHING returns zero rows
-- when an alert with the same dedup_key already exists — the caller maps
-- that to "skip send". Concurrent detectors arriving at the same finding
-- still produce exactly one row.
INSERT INTO polymarket_alerts (
    dedup_key, strategy_version,
    kind, reason, severity,
    market_id, trader_id, trade_id,
    payload, status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending')
ON CONFLICT (dedup_key) DO NOTHING
RETURNING *;

-- name: ClaimPendingAlertsForSend :many
-- Atomically transition up to $1 pending alerts to 'sending' and return
-- them. The inner SELECT … FOR UPDATE SKIP LOCKED guarantees concurrent
-- senders see disjoint batches even though the outer UPDATE runs in its
-- own short-lived transaction. A row in `sending` is invisible to the
-- next claimer until MarkAlertSent / MarkAlertSendFailed advances it, or
-- ResetStaleSendingAlerts recovers it after a crashed sender.
UPDATE polymarket_alerts
SET status     = 'sending',
    updated_at = NOW()
WHERE id IN (
    SELECT id FROM polymarket_alerts
    WHERE status = 'pending'
    ORDER BY created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkAlertSent :exec
-- Completes a claim. Guarded by status='sending' so a duplicate sender
-- racing on a row that was already MarkFailed cannot resurrect it.
UPDATE polymarket_alerts
SET status              = 'sent',
    sent_at             = NOW(),
    telegram_message_id = $2,
    last_send_error     = NULL,
    updated_at          = NOW()
WHERE id     = $1
  AND status = 'sending';

-- name: MarkAlertSendFailed :exec
-- Releases the claim back to 'pending' so the next ClaimPending tick can
-- retry. The send_attempts column is bumped + the error message stored so
-- operators can spot poison rows.
UPDATE polymarket_alerts
SET status          = 'pending',
    send_attempts   = send_attempts + 1,
    last_send_error = $2,
    updated_at      = NOW()
WHERE id     = $1
  AND status = 'sending';

-- name: ResetStaleSendingAlerts :exec
-- Crash recovery: any row that's been in 'sending' for longer than the
-- supplied cutoff is moved back to 'pending'. Called by the sender worker
-- on each tick (cheap when zero rows match).
UPDATE polymarket_alerts
SET status     = 'pending',
    updated_at = NOW()
WHERE status     = 'sending'
  AND updated_at < $1;

-- name: AlertExistsByDedupKey :one
SELECT EXISTS (
    SELECT 1 FROM polymarket_alerts WHERE dedup_key = $1
) AS exists;

-- name: LatestClusterAlertForCategory :one
-- Used by the cluster cooldown gate. Returns NULL when there's been no
-- cluster alert for this market+outcome under the current strategy.
SELECT * FROM polymarket_alerts
WHERE kind             = 'category_watch'
  AND market_id        = $1
  AND strategy_version = $2
ORDER BY created_at DESC
LIMIT 1;
