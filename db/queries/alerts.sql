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
-- Pull a batch of pending alerts for the sender worker. SKIP LOCKED so
-- two senders running in parallel never pick the same row.
SELECT * FROM polymarket_alerts
WHERE status = 'pending'
ORDER BY created_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkAlertSent :exec
UPDATE polymarket_alerts
SET status              = 'sent',
    sent_at             = NOW(),
    telegram_message_id = $2,
    last_send_error     = NULL,
    updated_at          = NOW()
WHERE id = $1;

-- name: MarkAlertSendFailed :exec
-- Leave status='pending' for retry; bump attempts and record the error.
UPDATE polymarket_alerts
SET send_attempts   = send_attempts + 1,
    last_send_error = $2,
    updated_at      = NOW()
WHERE id = $1;

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
