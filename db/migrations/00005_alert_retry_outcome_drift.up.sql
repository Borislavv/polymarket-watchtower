-- 00005_alert_retry_outcome_drift.up.sql
--
-- Hardening pass: alert retry policy + post-alert outcome tracking +
-- CLV-lite drift enrichment. All three live on polymarket_alerts so the
-- query path stays simple — three independent worker scans hitting the
-- same row by id, no cross-table joins.
--
-- ALERT RETRY
-- -----------
-- `send_attempts` already exists. Add:
--   next_retry_at  — when MarkAlertSendFailed schedules the next attempt
--                    (exponential backoff + jitter, NULL for permanent
--                    failures or rows not eligible for retry).
--   last_attempt_at — bookkeeping for observability + stale-claim recovery.
-- The claim query is extended in 00005 sqlc to pick up
--   status='pending' OR (status='failed' AND next_retry_at <= now()).
--
-- OUTCOME TRACKING
-- ----------------
-- After Telegram delivery succeeds, the outcomes worker periodically
-- checks the market's upstream resolution state and stamps:
--   outcome_status        — pending|resolved_correct|resolved_wrong|unknown|unavailable
--   outcome_checked_at    — when the worker last touched this row
--   resolved_at           — when Polymarket resolved the market
--   winning_outcome_token — the CLOB token id that resolved to 1.0
--   winning_outcome_label — human label for the winning side
-- We never modify alert decision logic from this path; outcome tracking
-- is signal-quality measurement only.
--
-- CLV-LITE DRIFT
-- --------------
-- After Telegram delivery succeeds, the drift worker waits for each
-- reference window to elapse and persists the price drift relative to
-- the alert's trade price. Signed so that:
--   * BUY YES (or BUY <outcome>) → positive drift means favorable
--   * SELL YES (or SELL <outcome>) → negative drift means favorable
-- The worker normalises to "favorable basis points" by multiplying by
-- the alert direction; values stored unsigned (basis points 0..10000)
-- when the price moved favorably, negative otherwise.
-- drift_status: pending|available|unavailable.
--
-- Worker scans are bounded by partial indexes so the queries stay cheap
-- even with millions of alert rows.

BEGIN;

ALTER TABLE polymarket_alerts
    ADD COLUMN next_retry_at         TIMESTAMPTZ NULL,
    ADD COLUMN last_attempt_at       TIMESTAMPTZ NULL,
    ADD COLUMN outcome_status        TEXT        NOT NULL DEFAULT 'pending',
    ADD COLUMN outcome_checked_at    TIMESTAMPTZ NULL,
    ADD COLUMN resolved_at           TIMESTAMPTZ NULL,
    ADD COLUMN winning_outcome_token TEXT        NULL,
    ADD COLUMN winning_outcome_label TEXT        NULL,
    ADD COLUMN drift_status          TEXT        NOT NULL DEFAULT 'pending',
    ADD COLUMN drift_checked_at      TIMESTAMPTZ NULL,
    ADD COLUMN clv_15m               DOUBLE PRECISION NULL,
    ADD COLUMN clv_1h                DOUBLE PRECISION NULL,
    ADD COLUMN clv_6h                DOUBLE PRECISION NULL,
    ADD COLUMN clv_24h               DOUBLE PRECISION NULL,
    ADD CONSTRAINT polymarket_alerts_outcome_status_valid CHECK (
        outcome_status IN ('pending','resolved_correct','resolved_wrong','unknown','unavailable')
    ),
    ADD CONSTRAINT polymarket_alerts_drift_status_valid CHECK (
        drift_status IN ('pending','available','unavailable')
    );

-- Retryable failed rows scan: drives ClaimPendingAlertsForSend's UNION.
CREATE INDEX idx_alerts_failed_retry
    ON polymarket_alerts(next_retry_at)
    WHERE status = 'failed' AND next_retry_at IS NOT NULL;

-- Outcome worker scan: sent alerts whose outcome is still pending.
CREATE INDEX idx_alerts_outcome_pending
    ON polymarket_alerts(outcome_checked_at NULLS FIRST, id)
    WHERE status = 'sent' AND outcome_status = 'pending';

-- Drift worker scan: sent alerts whose drift is still pending.
CREATE INDEX idx_alerts_drift_pending
    ON polymarket_alerts(created_at, id)
    WHERE status = 'sent' AND drift_status = 'pending';

COMMIT;
