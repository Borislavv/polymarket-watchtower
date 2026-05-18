-- 00008_signal_reports.up.sql
--
-- DB-backed scheduler state for the signal-quality reports worker
-- (daily / weekly / monthly / quarterly / yearly). Every send produces
-- ONE row, identified by a dedup_key
--
--     signal-report:<period_type>:<period_start>:<period_end>
--
-- A restart-safe scheduler inserts the row in `pending` state, sends to
-- Telegram, then flips to `sent` with the upstream message_id. A
-- concurrent process racing on the same period collides on the UNIQUE
-- constraint and skips silently — exactly one report per period.
--
-- payload is the rendered Telegram body (JSON-encoded so the operator
-- can re-render or diff historical reports without re-querying the
-- alerts table).

BEGIN;

CREATE TABLE polymarket_signal_reports (
    id                  BIGSERIAL PRIMARY KEY,
    period_type         TEXT             NOT NULL,
    period_start        TIMESTAMPTZ      NOT NULL,
    period_end          TIMESTAMPTZ      NOT NULL,
    scheduled_at        TIMESTAMPTZ      NOT NULL,
    sent_at             TIMESTAMPTZ,
    status              TEXT             NOT NULL DEFAULT 'pending',
    telegram_message_id BIGINT,
    last_error          TEXT,
    payload             JSONB,
    dedup_key           TEXT             NOT NULL UNIQUE,
    created_at          TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ      NOT NULL DEFAULT NOW(),

    CONSTRAINT polymarket_signal_reports_period_type_valid
        CHECK (period_type IN ('daily','weekly','monthly','quarterly','yearly')),
    CONSTRAINT polymarket_signal_reports_status_valid
        CHECK (status IN ('pending','sent','failed'))
);

CREATE INDEX idx_signal_reports_period
    ON polymarket_signal_reports (period_type, period_end DESC);

COMMIT;
