-- name: InsertAlertAnalysis :one
-- Persist one AI alert-analysis row. Versions are append-only; the
-- usecase layer chooses the version number (latest+1 on refresh,
-- else 1). The trigger fields record WHY this version was generated
-- so the lessons dataset can be re-derived later.
INSERT INTO polymarket_alert_analyses (
    alert_id, version, trigger_kind, trigger_detail, model,
    prompt_chars, output_chars, prompt_tokens, completion_tokens,
    estimated_cost_usd, analysis_text, verdict, status, last_error
) VALUES (
    sqlc.arg(alert_id)::bigint,
    sqlc.arg(version)::int,
    sqlc.arg(trigger_kind)::text,
    sqlc.narg(trigger_detail)::text,
    sqlc.arg(model)::text,
    sqlc.arg(prompt_chars)::int,
    sqlc.arg(output_chars)::int,
    sqlc.arg(prompt_tokens)::int,
    sqlc.arg(completion_tokens)::int,
    sqlc.arg(estimated_cost_usd)::double precision,
    sqlc.arg(analysis_text)::text,
    sqlc.narg(verdict)::text,
    sqlc.arg(status)::text,
    sqlc.narg(last_error)::text
)
ON CONFLICT (alert_id, version) DO NOTHING
RETURNING *;

-- name: LatestAlertAnalysisVersion :one
-- Returns the highest version number recorded for the alert, or 0
-- when none. Used by the refresh policy to compute next-version.
SELECT COALESCE(MAX(version), 0)::int FROM polymarket_alert_analyses
WHERE alert_id = sqlc.arg(alert_id)::bigint;

-- name: LatestAlertAnalysis :one
-- Returns the most recent analysis row for the alert. Telegram
-- formatter and operators reading the alert page consume this.
SELECT * FROM polymarket_alert_analyses
WHERE alert_id = sqlc.arg(alert_id)::bigint
ORDER BY version DESC
LIMIT 1;

-- name: InsertMarketIntelligenceReport :one
-- Persist one 2h report. summary_hash unique-conflict skips silently
-- so the caller can decide whether to retry-with-fresh-content.
INSERT INTO polymarket_market_intelligence_reports (
    period_start, period_end, summary_hash, report_text, markets_json,
    model, prompt_tokens, completion_tokens, estimated_cost_usd,
    telegram_message_id, telegram_chat_id, delivery_status, last_delivery_error
) VALUES (
    sqlc.arg(period_start)::timestamptz,
    sqlc.arg(period_end)::timestamptz,
    sqlc.arg(summary_hash)::text,
    sqlc.arg(report_text)::text,
    sqlc.arg(markets_json)::jsonb,
    sqlc.arg(model)::text,
    sqlc.arg(prompt_tokens)::int,
    sqlc.arg(completion_tokens)::int,
    sqlc.arg(estimated_cost_usd)::double precision,
    sqlc.narg(telegram_message_id)::bigint,
    sqlc.narg(telegram_chat_id)::text,
    sqlc.arg(delivery_status)::text,
    sqlc.narg(last_delivery_error)::text
)
ON CONFLICT (summary_hash) DO NOTHING
RETURNING *;

-- name: LatestMarketIntelligenceReport :one
SELECT * FROM polymarket_market_intelligence_reports
ORDER BY generated_at DESC
LIMIT 1;

-- name: InsertAlertOutcomeAnalysis :one
-- One row per alert; the outcomes worker calls this once per alert
-- when the market resolves. Unique constraint on alert_id makes the
-- write idempotent.
INSERT INTO polymarket_alert_outcome_analyses (
    alert_id, outcome_status, won_expected,
    ai_reason_text, ai_lessons_text, confidence,
    model, prompt_tokens, completion_tokens, estimated_cost_usd,
    telegram_message_id, telegram_chat_id, delivery_status, last_delivery_error
) VALUES (
    sqlc.arg(alert_id)::bigint,
    sqlc.arg(outcome_status)::text,
    sqlc.narg(won_expected)::bool,
    sqlc.arg(ai_reason_text)::text,
    sqlc.narg(ai_lessons_text)::text,
    sqlc.arg(confidence)::double precision,
    sqlc.arg(model)::text,
    sqlc.arg(prompt_tokens)::int,
    sqlc.arg(completion_tokens)::int,
    sqlc.arg(estimated_cost_usd)::double precision,
    sqlc.narg(telegram_message_id)::bigint,
    sqlc.narg(telegram_chat_id)::text,
    sqlc.arg(delivery_status)::text,
    sqlc.narg(last_delivery_error)::text
)
ON CONFLICT (alert_id) DO NOTHING
RETURNING *;

-- name: GetAlertOutcomeAnalysis :one
SELECT * FROM polymarket_alert_outcome_analyses
WHERE alert_id = sqlc.arg(alert_id)::bigint;
