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
-- Atomically transition up to $1 alerts to 'sending' and return them. The
-- inner SELECT … FOR UPDATE SKIP LOCKED guarantees concurrent senders see
-- disjoint batches even though the outer UPDATE runs in its own short-
-- lived transaction. A row in `sending` is invisible to the next claimer
-- until MarkAlertSent / MarkAlertSendFailed advances it, or
-- ResetStaleSendingAlerts recovers it after a crashed sender.
--
-- v4 hardening: the claim picks up BOTH pending rows AND retry-eligible
-- failed rows (status='failed' AND next_retry_at <= now()). The retry
-- worker is the same alertsender; there is no separate retry path.
UPDATE polymarket_alerts
SET status     = 'sending',
    updated_at = NOW()
WHERE id IN (
    SELECT id FROM polymarket_alerts
    WHERE status = 'pending'
       OR (status = 'failed' AND next_retry_at IS NOT NULL AND next_retry_at <= NOW())
    ORDER BY created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkAlertSent :exec
-- Completes a claim. Guarded by status='sending' so a duplicate sender
-- racing on a row that was already MarkFailed cannot resurrect it. Clears
-- retry state so a row that succeeded after a failure does not keep its
-- schedule.
UPDATE polymarket_alerts
SET status              = 'sent',
    sent_at             = NOW(),
    last_attempt_at     = NOW(),
    telegram_message_id = $2,
    last_send_error     = NULL,
    next_retry_at       = NULL,
    updated_at          = NOW()
WHERE id     = $1
  AND status = 'sending';

-- name: MarkAlertSendFailed :exec
-- Records a failed delivery attempt. The caller has already computed
-- next_retry_at (NULL = exhausted / permanent; non-NULL = retryable
-- at the supplied wall-clock time). Status flips to 'failed' regardless
-- — the claim query picks up retryable-failed rows on the next tick.
UPDATE polymarket_alerts
SET status          = 'failed',
    send_attempts   = send_attempts + 1,
    last_attempt_at = NOW(),
    last_send_error = $2,
    next_retry_at   = $3,
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

-- name: ListSentAlertsForOutcomeCheck :many
-- Used by the outcomes worker. Returns sent alerts whose outcome is
-- still pending, prioritised by oldest-unchecked-first so a single
-- alert that keeps failing the upstream lookup doesn't starve fresher
-- candidates. The query joins polymarket_markets to filter for markets
-- whose end_date has passed (or which are flagged closed) — we only
-- check markets that could plausibly be resolved.
SELECT a.*
FROM polymarket_alerts a
JOIN polymarket_markets m ON m.id = a.market_id
WHERE a.status         = 'sent'
  AND a.outcome_status = 'pending'
  AND (m.closed = TRUE OR (m.end_date IS NOT NULL AND m.end_date <= NOW()))
ORDER BY a.outcome_checked_at NULLS FIRST, a.id
LIMIT sqlc.arg(claim_limit)::integer;

-- name: MarkAlertOutcome :exec
-- Stamps the outcome verdict on a sent alert. Called by the outcomes
-- worker after it has consulted Polymarket's market state. status must
-- be one of: resolved_correct, resolved_wrong, unknown, unavailable.
UPDATE polymarket_alerts
SET outcome_status        = sqlc.arg(outcome_status)::text,
    outcome_checked_at    = NOW(),
    resolved_at           = sqlc.narg(resolved_at)::timestamptz,
    winning_outcome_token = sqlc.narg(winning_outcome_token)::text,
    winning_outcome_label = sqlc.narg(winning_outcome_label)::text,
    updated_at            = NOW()
WHERE id = $1;

-- name: MarkAlertOutcomeUnavailableTouch :exec
-- Bumps outcome_checked_at on alerts where the upstream check failed
-- transiently (e.g. Polymarket /markets/{id} returned an error or a
-- not-yet-resolved market). Keeps the row scheduled for re-check at the
-- next tick without changing the verdict.
UPDATE polymarket_alerts
SET outcome_checked_at = NOW(),
    updated_at         = NOW()
WHERE id = $1;

-- name: ListAlertsForReaction :many
-- Returns sent alerts with a known outcome that haven't yet had a
-- Telegram reaction applied. The index idx_alerts_reaction_pending
-- (migration 00007) makes this a partial-index scan. Ordering by
-- resolved_at keeps the reactor processing newest-resolution-first so
-- recent reactions appear before historical backfill.
SELECT a.*
FROM polymarket_alerts a
WHERE a.status                   = 'sent'
  AND a.telegram_message_id     IS NOT NULL
  AND a.outcome_status          IN ('resolved_correct','resolved_wrong','unknown')
  AND a.telegram_reaction_status IN ('pending','failed')
ORDER BY a.resolved_at DESC NULLS LAST, a.id
LIMIT sqlc.arg(claim_limit)::integer;

-- name: MarkAlertReactionApplied :exec
-- Stamps a successful setMessageReaction result on the alert row.
-- Status MUST be one of the CHECK-constrained values
-- (applied/unsupported/failed/disabled); the caller maps the verdict.
UPDATE polymarket_alerts
SET telegram_reaction_status = sqlc.arg(status)::text,
    telegram_reaction_emoji  = sqlc.narg(emoji)::text,
    last_reaction_at         = CASE
                                  WHEN sqlc.arg(status)::text = 'applied' THEN NOW()
                                  ELSE last_reaction_at
                               END,
    updated_at               = NOW()
WHERE id = $1;

-- name: ListSentAlertsForDrift :many
-- Used by the drift worker. Returns sent alerts whose drift is still
-- pending AND whose oldest reference window (15m by convention) has
-- already elapsed. Bounded by claim_limit per tick.
SELECT a.*
FROM polymarket_alerts a
WHERE a.status       = 'sent'
  AND a.drift_status = 'pending'
  AND a.sent_at IS NOT NULL
  AND a.sent_at + sqlc.arg(min_window)::interval <= NOW()
ORDER BY a.sent_at NULLS LAST, a.id
LIMIT sqlc.arg(claim_limit)::integer;

-- name: MarkAlertDrift :exec
-- Persists the four CLV-lite windows. A NULL CLV column means the
-- reference price for that window was unavailable (e.g. no later trade
-- on the same (market, outcome)). drift_status flips to 'available'
-- when at least one window produced a number; 'unavailable' otherwise.
UPDATE polymarket_alerts
SET drift_status      = sqlc.arg(drift_status)::text,
    drift_checked_at  = NOW(),
    clv_15m           = sqlc.narg(clv_15m)::double precision,
    clv_1h            = sqlc.narg(clv_1h)::double precision,
    clv_6h            = sqlc.narg(clv_6h)::double precision,
    clv_24h           = sqlc.narg(clv_24h)::double precision,
    updated_at        = NOW()
WHERE id = $1;

-- name: TradePriceAtOrAfter :one
-- Returns the price of the FIRST trade on (market, outcome_token) at or
-- after the supplied timestamp. NULL when no later trade exists yet.
-- Powers the CLV-lite drift worker's per-window reference price lookup.
SELECT price::double precision AS price
FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND traded_at     >= sqlc.arg(at_or_after)::timestamptz
ORDER BY traded_at ASC
LIMIT 1;

-- name: GetAlertByID :one
-- Single-row fetch used by the outcome-learning worker to reload
-- the full row (payload, telegram_message_id, outcome_status, etc.)
-- before invoking the AI postmortem path.
SELECT * FROM polymarket_alerts WHERE id = $1;

-- name: ListResolvedAlertsForPostmortem :many
-- Returns sent alerts whose outcome is terminal AND which do NOT
-- yet have an outcome-analysis row. The LEFT JOIN keeps the query
-- a single roundtrip per claim cycle.
--
-- We process newest-first so a resolution burst (e.g. an event
-- night) gets postmortems for the most-recent settlements first.
SELECT a.*
FROM polymarket_alerts a
LEFT JOIN polymarket_alert_outcome_analyses o ON o.alert_id = a.id
WHERE a.status         = 'sent'
  AND a.outcome_status IN ('resolved_correct', 'resolved_wrong')
  AND a.resolved_at   IS NOT NULL
  AND o.id            IS NULL
ORDER BY a.resolved_at DESC NULLS LAST, a.id DESC
LIMIT sqlc.arg(limit_count)::integer;
