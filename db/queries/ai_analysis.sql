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
-- Persist one 2h report. period_key UNIQUE — two ticks landing in the
-- same bucket (computed deterministically in the worker) collapse to
-- a single row, eliminating the duplicate-Telegram-send class of bug
-- that prompted migration 00014.
INSERT INTO polymarket_market_intelligence_reports (
    period_key, period_start, period_end, summary_hash, report_text, markets_json,
    model, prompt_tokens, completion_tokens, estimated_cost_usd,
    telegram_message_id, telegram_chat_id, delivery_status, last_delivery_error
) VALUES (
    sqlc.arg(period_key)::text,
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
ON CONFLICT (period_key) DO NOTHING
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

-- name: ListMarketIntelligenceCandidates :many
-- Top-N candidate markets for the 2h intelligence report. Selection
-- philosophy: deep into lifecycle + recent activity + non-trivial
-- liquidity. The query is intentionally simple — the AI does the
-- ranking; we provide a generous shortlist.
SELECT
    m.condition_id,
    m.question,
    c.name AS category,
    (100.0 * EXTRACT(EPOCH FROM (NOW() - m.start_date)) /
            NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0))::double precision AS lifecycle_pct,
    -- last 24h aggregates over polymarket_trades for this market
    COALESCE(
      (SELECT COUNT(*) FROM polymarket_trades t
       WHERE t.market_id = m.id AND t.traded_at >= NOW() - INTERVAL '24 hours'),
      0)::bigint AS trades_24h,
    COALESCE(
      (SELECT SUM(t.notional_usd) FROM polymarket_trades t
       WHERE t.market_id = m.id AND t.traded_at >= NOW() - INTERVAL '24 hours'),
      0)::double precision AS volume_24h_usd,
    -- last observed price on the first outcome token (proxy probability)
    COALESCE(
      (SELECT t.price FROM polymarket_trades t
       WHERE t.market_id = m.id
       ORDER BY t.traded_at DESC LIMIT 1),
      0)::double precision AS last_price,
    -- alerts emitted on this market in the last 24h
    COALESCE(
      (SELECT COUNT(*) FROM polymarket_alerts a
       WHERE a.market_id = m.id AND a.created_at >= NOW() - INTERVAL '24 hours'),
      0)::bigint AS alerts_24h
FROM polymarket_markets m
LEFT JOIN polymarket_market_categories mc ON mc.market_id = m.id
LEFT JOIN polymarket_categories c ON c.id = mc.category_id
WHERE m.active = TRUE
  AND m.deleted_at IS NULL
  AND m.purged_at  IS NULL
  AND m.start_date IS NOT NULL
  AND m.end_date   IS NOT NULL
  AND m.end_date    > NOW()
ORDER BY (100.0 * EXTRACT(EPOCH FROM (NOW() - m.start_date)) /
                NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0))::double precision DESC NULLS LAST,
         volume_24h_usd DESC
LIMIT sqlc.arg(limit_count)::integer;

-- name: UpsertAlertStrategyDimensions :exec
-- One row per alert; idempotent. Called by the alertsender worker
-- BEFORE Telegram delivery so the attribution row exists by the
-- time the operator sees the alert. Overwrites any prior bucketing
-- (so a re-run after schema fix doesn't accumulate ghosts).
INSERT INTO polymarket_alert_strategy_dimensions (
    alert_id, strategy_family, lifecycle_bucket, odds_bucket,
    notional_bucket, return_bucket, category, accumulation_window,
    ownership_share_bucket, volatility_regime, new_wallet,
    quiet_market, dormant_wallet, drift_regime, ai_verdict
) VALUES (
    sqlc.arg(alert_id)::bigint,
    sqlc.arg(strategy_family)::text,
    sqlc.arg(lifecycle_bucket)::text,
    sqlc.narg(odds_bucket)::text,
    sqlc.narg(notional_bucket)::text,
    sqlc.narg(return_bucket)::text,
    sqlc.narg(category)::text,
    sqlc.narg(accumulation_window)::text,
    sqlc.narg(ownership_share_bucket)::text,
    sqlc.narg(volatility_regime)::text,
    sqlc.arg(new_wallet)::bool,
    sqlc.arg(quiet_market)::bool,
    sqlc.arg(dormant_wallet)::bool,
    sqlc.narg(drift_regime)::text,
    sqlc.narg(ai_verdict)::text
)
ON CONFLICT (alert_id) DO UPDATE SET
    strategy_family       = EXCLUDED.strategy_family,
    lifecycle_bucket      = EXCLUDED.lifecycle_bucket,
    odds_bucket           = EXCLUDED.odds_bucket,
    notional_bucket       = EXCLUDED.notional_bucket,
    return_bucket         = EXCLUDED.return_bucket,
    category              = EXCLUDED.category,
    accumulation_window   = EXCLUDED.accumulation_window,
    ownership_share_bucket = EXCLUDED.ownership_share_bucket,
    volatility_regime     = EXCLUDED.volatility_regime,
    new_wallet            = EXCLUDED.new_wallet,
    quiet_market          = EXCLUDED.quiet_market,
    dormant_wallet        = EXCLUDED.dormant_wallet,
    drift_regime          = EXCLUDED.drift_regime,
    ai_verdict            = EXCLUDED.ai_verdict;
