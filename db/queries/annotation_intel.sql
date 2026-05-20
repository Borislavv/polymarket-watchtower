-- name: UpsertEventAnnotationRanking :exec
-- Idempotent insert keyed on (period_start, event_slug, annotation_hash).
INSERT INTO polymarket_event_annotation_rankings (
    period_start, period_end, event_slug, market_slug, annotation_hash,
    rank, importance, volatility_potential, probability_impact,
    affected_outcome, title, reason, market_read
) VALUES (
    @period_start, @period_end, @event_slug, @market_slug, @annotation_hash,
    @rank, @importance, @volatility_potential, @probability_impact,
    @affected_outcome, @title, @reason, @market_read
)
ON CONFLICT (period_start, event_slug, annotation_hash) DO UPDATE SET
    period_end           = EXCLUDED.period_end,
    market_slug          = COALESCE(EXCLUDED.market_slug, polymarket_event_annotation_rankings.market_slug),
    rank                 = EXCLUDED.rank,
    importance           = EXCLUDED.importance,
    volatility_potential = EXCLUDED.volatility_potential,
    probability_impact   = EXCLUDED.probability_impact,
    affected_outcome     = COALESCE(EXCLUDED.affected_outcome, polymarket_event_annotation_rankings.affected_outcome),
    title                = EXCLUDED.title,
    reason               = EXCLUDED.reason,
    market_read          = EXCLUDED.market_read;

-- name: ListLatestRankingForPeriod :many
SELECT
    id, period_start, period_end, event_slug, market_slug, annotation_hash,
    rank, importance, volatility_potential, probability_impact,
    affected_outcome, title, reason, market_read, created_at
FROM polymarket_event_annotation_rankings
WHERE period_start = @period_start
ORDER BY rank, importance DESC;

-- name: UpsertDailyPoliticalIntelReport :one
INSERT INTO polymarket_daily_political_intel_reports (
    report_date, period_start, period_end,
    selected_markets_json, selected_annotations_json, catalysts_json,
    ai_report_text, telegram_message_ids_json, delivery_status, last_delivery_error,
    updated_at
) VALUES (
    @report_date, @period_start, @period_end,
    @selected_markets_json, @selected_annotations_json, @catalysts_json,
    @ai_report_text, @telegram_message_ids_json, @delivery_status, @last_delivery_error,
    NOW()
)
ON CONFLICT (report_date) DO UPDATE SET
    period_start              = EXCLUDED.period_start,
    period_end                = EXCLUDED.period_end,
    selected_markets_json     = EXCLUDED.selected_markets_json,
    selected_annotations_json = EXCLUDED.selected_annotations_json,
    catalysts_json            = EXCLUDED.catalysts_json,
    ai_report_text            = COALESCE(NULLIF(EXCLUDED.ai_report_text, ''), polymarket_daily_political_intel_reports.ai_report_text),
    telegram_message_ids_json = EXCLUDED.telegram_message_ids_json,
    delivery_status           = EXCLUDED.delivery_status,
    last_delivery_error       = EXCLUDED.last_delivery_error,
    updated_at                = NOW()
RETURNING id;

-- name: GetDailyPoliticalIntelReport :one
SELECT
    id, report_date, period_start, period_end,
    selected_markets_json, selected_annotations_json, catalysts_json,
    ai_report_text, telegram_message_ids_json, delivery_status, last_delivery_error,
    created_at, updated_at
FROM polymarket_daily_political_intel_reports
WHERE report_date = @report_date;
