-- 00015_ai_request_logs — operational log of every AI provider
-- interaction (success, skip, failure). Decouples analytical content
-- from operational telemetry:
--
--   - polymarket_alert_analyses               — SUCCESSFUL AI answers ONLY.
--     analysis_text contains real model output. Error/skipped rows
--     are no longer written here (the v8 incident: raw OpenAI 429
--     JSON was landing in alert_analyses.last_error, making the
--     analytical table unusable for dashboards).
--
--   - polymarket_ai_request_logs (this table) — every provider call:
--     successful, skipped, failed. Holds short sanitized error
--     metadata for SQL debugging; no full prompts, no raw provider
--     bodies (those would never fit in a useful dashboard and may
--     contain user/PII the operator doesn't want long-lived).
--
-- target_kind groups requests for dashboards (alert vs market
-- intelligence vs outcome). target_id is the upstream row id when
-- applicable; null when the request is not tied to a single row
-- (e.g. a periodic intelligence sweep that produces a single new
-- report row).
--
-- status enum (TEXT, validated in code):
--   success                  – call returned usable text
--   failed_retryable         – transient (5xx, network, timeout)
--   failed_terminal          – permanent (400, validation rejected)
--   skipped_budget           – daily budget exhausted
--   skipped_rate_limit       – local rate-limit gate fired
--   skipped_quota            – provider 429 insufficient_quota
--   skipped_disabled         – AlertsEnabled / web disabled
--   skipped_no_api_key       – key not configured
--
-- error_category (TEXT, short, stable):
--   quota_exceeded | rate_limited | timeout | provider_5xx |
--   bad_request | prompt_rejected | invalid_model | validation_failed |
--   disabled | budget_exhausted | no_api_key | unknown
--
-- error_message is capped at 500 chars by the writer to keep the
-- table compact. We never store the raw provider body.

CREATE TABLE polymarket_ai_request_logs (
    id                  BIGSERIAL PRIMARY KEY,
    target_kind         TEXT        NOT NULL,
    target_id           BIGINT      NULL,
    provider            TEXT        NOT NULL,
    model               TEXT        NOT NULL,
    request_kind        TEXT        NOT NULL,
    status              TEXT        NOT NULL,
    error_category      TEXT        NULL,
    error_code          TEXT        NULL,
    error_message       TEXT        NULL,
    http_status         INT         NULL,
    prompt_chars        INT         NOT NULL DEFAULT 0,
    output_chars        INT         NOT NULL DEFAULT 0,
    prompt_tokens       INT         NOT NULL DEFAULT 0,
    completion_tokens   INT         NOT NULL DEFAULT 0,
    estimated_cost_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,
    latency_ms          BIGINT      NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_ai_request_logs_created_at
    ON polymarket_ai_request_logs (created_at DESC);
CREATE INDEX idx_ai_request_logs_target
    ON polymarket_ai_request_logs (target_kind, target_id);
CREATE INDEX idx_ai_request_logs_status
    ON polymarket_ai_request_logs (status, error_category);

-- Mark existing junk rows in polymarket_alert_analyses for review.
-- A row with Status='error' and empty analysis_text was always a
-- provider failure being stored as analysis (the bug). We do NOT
-- delete history; a new column lets operators ignore them in queries.
ALTER TABLE polymarket_alert_analyses
    ADD COLUMN IF NOT EXISTS legacy_provider_failure BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE polymarket_alert_analyses
   SET legacy_provider_failure = TRUE
 WHERE legacy_provider_failure = FALSE
   AND (status = 'error' OR status = 'skipped')
   AND (analysis_text IS NULL OR analysis_text = '');

CREATE INDEX IF NOT EXISTS idx_alert_analyses_legacy_failure
    ON polymarket_alert_analyses (legacy_provider_failure)
    WHERE legacy_provider_failure = TRUE;
