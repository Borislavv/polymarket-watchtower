-- name: TryCreatePendingSignalReport :one
-- Idempotent claim primitive for the signal-quality reports scheduler.
-- A worker that has decided "it is time to send the daily report for
-- 2026-05-17" calls this with the canonical dedup_key
--   signal-report:<period_type>:<period_start>:<period_end>
-- The UNIQUE constraint on dedup_key collapses concurrent claims to
-- exactly one row; ON CONFLICT DO NOTHING + the RETURNING-or-empty
-- pattern means the caller can distinguish "I won" (row returned)
-- from "someone else already inserted" (no row). Restart-safe by
-- construction.
INSERT INTO polymarket_signal_reports (
    period_type, period_start, period_end, scheduled_at,
    status, payload, dedup_key
) VALUES (
    sqlc.arg(period_type)::text,
    sqlc.arg(period_start)::timestamptz,
    sqlc.arg(period_end)::timestamptz,
    sqlc.arg(scheduled_at)::timestamptz,
    'pending',
    sqlc.narg(payload)::jsonb,
    sqlc.arg(dedup_key)::text
)
ON CONFLICT (dedup_key) DO NOTHING
RETURNING id;

-- name: MarkSignalReportSent :exec
-- Flips the row to 'sent' and persists the upstream Telegram message
-- id. Called once per successful send. last_error is intentionally
-- cleared on success so a row that recovered after a transient failure
-- doesn't keep its stale error text.
UPDATE polymarket_signal_reports
SET status              = 'sent',
    sent_at             = NOW(),
    telegram_message_id = sqlc.narg(telegram_message_id)::bigint,
    last_error          = NULL,
    updated_at          = NOW()
WHERE id = $1;

-- name: MarkSignalReportFailed :exec
-- Captures a send failure on the row. The scheduler treats failed rows
-- as permanently failed for the period (idempotency over correctness:
-- a flapping Telegram send is far worse than a missed report).
UPDATE polymarket_signal_reports
SET status     = 'failed',
    last_error = sqlc.arg(last_error)::text,
    updated_at = NOW()
WHERE id = $1;

-- name: SignalQualityAggregate :one
-- One-shot aggregate that powers the daily/weekly/monthly/quarterly/yearly
-- reports. Returns total counts, resolved counts, success/failure
-- counts, and the CLV-summary fields — all in a single roundtrip so
-- the renderer stays pure and the worker stays fast.
SELECT
    COUNT(*)::bigint                                                                           AS total_alerts,
    COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct')::bigint                       AS success_count,
    COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong')::bigint                         AS failure_count,
    COUNT(*) FILTER (WHERE outcome_status = 'unknown')::bigint                                AS ambiguous_count,
    COUNT(*) FILTER (WHERE outcome_status = 'unavailable')::bigint                            AS unavailable_count,
    COUNT(*) FILTER (WHERE outcome_status = 'pending')::bigint                                AS pending_count,
    COALESCE(AVG(clv_24h) FILTER (WHERE clv_24h IS NOT NULL), 0)::double precision            AS avg_clv_24h,
    COUNT(*) FILTER (WHERE clv_24h IS NOT NULL AND clv_24h > 0)::bigint                       AS positive_clv_24h_count,
    COUNT(*) FILTER (WHERE clv_24h IS NOT NULL)::bigint                                       AS clv_24h_sample_count
FROM polymarket_alerts
WHERE sent_at IS NOT NULL
  AND sent_at >= sqlc.arg(period_start)::timestamptz
  AND sent_at <  sqlc.arg(period_end)::timestamptz;

-- name: SignalQualityByKind :many
-- Per-kind breakdown for the report's "by kind" section.
SELECT
    kind                                                                AS kind,
    COUNT(*)::bigint                                                    AS total,
    COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct')::bigint AS success,
    COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong')::bigint   AS failure,
    COUNT(*) FILTER (WHERE outcome_status IN ('pending','unavailable','unknown'))::bigint AS unresolved
FROM polymarket_alerts
WHERE sent_at IS NOT NULL
  AND sent_at >= sqlc.arg(period_start)::timestamptz
  AND sent_at <  sqlc.arg(period_end)::timestamptz
GROUP BY kind
ORDER BY total DESC;

-- name: SignalQualityBySeverity :many
-- Per-severity breakdown for the report's "by severity" section.
SELECT
    severity                                                            AS severity,
    COUNT(*)::bigint                                                    AS total,
    COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct')::bigint AS success,
    COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong')::bigint   AS failure,
    COUNT(*) FILTER (WHERE outcome_status IN ('pending','unavailable','unknown'))::bigint AS unresolved
FROM polymarket_alerts
WHERE sent_at IS NOT NULL
  AND sent_at >= sqlc.arg(period_start)::timestamptz
  AND sent_at <  sqlc.arg(period_end)::timestamptz
GROUP BY severity
ORDER BY total DESC;

-- name: LatestSignalReportByPeriodType :one
-- Returns the most recent signal report of the given period_type, or
-- empty if none exists. Used at scheduler startup to decide whether
-- we missed a tick.
SELECT id, period_type, period_start, period_end, scheduled_at, sent_at,
       status, telegram_message_id, last_error, payload, dedup_key,
       created_at, updated_at
FROM polymarket_signal_reports
WHERE period_type = $1
ORDER BY period_end DESC
LIMIT 1;
