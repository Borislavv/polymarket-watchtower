-- v11.4 Strategy Learning Loop — read-model views.
--
-- These are query-only views aggregating polymarket_alerts +
-- polymarket_market_close_reviews so Grafana panels can render
-- the learning loop without copy/pasting SQL into every panel.
-- Append-only contract: views can be re-CREATED safely; the
-- underlying tables are never mutated.

-- 1. Market close review quality — one row per reviewed market.
CREATE OR REPLACE VIEW watchtower_market_close_review_quality_v AS
SELECT
    r.reviewed_at,
    r.market_id,
    r.condition_id,
    r.event_slug,
    COALESCE(m.question, '')                                                                            AS title,
    COALESCE(cat.name, '')                                                                              AS category,
    r.verdict,
    r.confidence,
    (SELECT COUNT(*) FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.status = 'sent' AND a.sent_at <= r.closed_at)             AS alert_count,
    (SELECT COUNT(*) FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.outcome_status = 'resolved_correct')                      AS confirmed_alert_count,
    (SELECT COUNT(*) FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.outcome_status = 'resolved_wrong')                        AS failed_alert_count,
    (SELECT COUNT(*) FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.outcome_status NOT IN ('resolved_correct','resolved_wrong'))
                                                                                                        AS ambiguous_alert_count,
    (SELECT AVG(a.clv_15m) FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.clv_15m IS NOT NULL)                                      AS avg_clv_15m,
    (SELECT AVG(a.clv_1h)  FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.clv_1h  IS NOT NULL)                                      AS avg_clv_1h,
    (SELECT AVG(a.clv_6h)  FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.clv_6h  IS NOT NULL)                                      AS avg_clv_6h,
    (SELECT AVG(a.clv_24h) FROM polymarket_alerts a
        WHERE a.market_id = r.market_id AND a.clv_24h IS NOT NULL)                                      AS avg_clv_24h,
    r.ai_model,
    r.input_tokens,
    r.output_tokens,
    r.estimated_cost_usd,
    r.status
FROM polymarket_market_close_reviews r
LEFT JOIN polymarket_markets m ON m.id = r.market_id
LEFT JOIN polymarket_market_categories mc ON mc.market_id = m.id
LEFT JOIN polymarket_categories cat ON cat.id = mc.category_id
WHERE r.status IN ('succeeded','failed','skipped');

-- 2. Strategy quality — one row per (strategy, kind, severity,
-- strategy_version). Aggregates ALL sent alerts whose markets
-- carry a succeeded review row, so the strategy verdict is
-- anchored on real resolutions.
CREATE OR REPLACE VIEW watchtower_strategy_quality_v AS
WITH reviewed AS (
    SELECT DISTINCT r.market_id, r.confidence
    FROM polymarket_market_close_reviews r
    WHERE r.status = 'succeeded'
)
SELECT
    CASE a.kind
        WHEN 'trade_anomaly'           THEN 'single_trade_whale'
        WHEN 'accumulation'            THEN 'accumulation'
        WHEN 'category_watch'          THEN 'cluster_convergence'
        WHEN 'ownership_concentration' THEN 'ownership_concentration'
        ELSE a.kind
    END                                                                                                AS strategy,
    a.kind                                                                                              AS alert_kind,
    a.severity,
    a.strategy_version,
    COUNT(*)                                                                                            AS reviewed_alert_count,
    COUNT(*) FILTER (WHERE a.outcome_status = 'resolved_correct')                                       AS confirmed_count,
    COUNT(*) FILTER (WHERE a.outcome_status = 'resolved_wrong')                                         AS false_positive_count,
    COUNT(*) FILTER (WHERE a.outcome_status NOT IN ('resolved_correct','resolved_wrong'))               AS inconclusive_count,
    AVG(a.clv_1h)                                                                                       AS avg_clv_1h,
    AVG(a.clv_6h)                                                                                       AS avg_clv_6h,
    AVG(a.clv_24h)                                                                                      AS avg_clv_24h,
    AVG(CASE WHEN a.clv_6h IS NOT NULL AND a.clv_6h > 0 THEN 1.0 ELSE 0.0 END)                          AS positive_clv_rate_6h,
    CASE WHEN COUNT(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')) = 0
         THEN NULL
         ELSE
             COUNT(*) FILTER (WHERE a.outcome_status = 'resolved_correct')::float8 /
             NULLIF(COUNT(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')), 0)
    END                                                                                                AS market_close_confirmed_rate,
    CASE
        WHEN COUNT(*) < 10                                                                              THEN 'needs_more_data'
        WHEN COUNT(*) FILTER (WHERE a.outcome_status = 'resolved_correct')::float8 /
             NULLIF(COUNT(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')), 0) > 0.65
                                                                                                       THEN 'keep'
        WHEN COUNT(*) FILTER (WHERE a.outcome_status = 'resolved_wrong')::float8 /
             NULLIF(COUNT(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')), 0) > 0.55
                                                                                                       THEN 'tighten'
        ELSE 'investigate'
    END                                                                                                AS verdict,
    'auto-derived from outcome + CLV6h sample size'                                                     AS verdict_reason
FROM polymarket_alerts a
JOIN reviewed r ON r.market_id = a.market_id
WHERE a.status = 'sent'
GROUP BY a.kind, a.severity, a.strategy_version;

-- 3. Reason / context-tag quality — one row per `reason` tag.
CREATE OR REPLACE VIEW watchtower_strategy_reason_quality_v AS
SELECT
    a.reason,
    a.kind                                                                                              AS base_strategy,
    COUNT(*)                                                                                            AS alert_count,
    CASE WHEN COUNT(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')) = 0
         THEN NULL
         ELSE
             COUNT(*) FILTER (WHERE a.outcome_status = 'resolved_correct')::float8 /
             NULLIF(COUNT(*) FILTER (WHERE a.outcome_status IN ('resolved_correct','resolved_wrong')), 0)
    END                                                                                                AS confirmed_rate,
    AVG(a.clv_6h)                                                                                       AS avg_clv_6h,
    CASE
        WHEN COUNT(*) < 10                                                                              THEN 'needs_more_data'
        WHEN AVG(a.clv_6h) > 0.01                                                                      THEN 'keep'
        WHEN AVG(a.clv_6h) < -0.01                                                                     THEN 'investigate'
        ELSE 'context_only'
    END                                                                                                AS verdict,
    'auto-derived from CLV6h sample size'                                                              AS verdict_reason
FROM polymarket_alerts a
WHERE a.status = 'sent' AND a.reason <> ''
GROUP BY a.reason, a.kind;

-- 4. AI cost / quality per surface / model / day.
CREATE OR REPLACE VIEW watchtower_ai_cost_quality_v AS
SELECT
    date_trunc('day', r.reviewed_at)                                                                    AS day,
    'market_close_review'                                                                               AS surface,
    r.ai_model                                                                                          AS model,
    COUNT(*)                                                                                            AS requests,
    COUNT(*) FILTER (WHERE r.status = 'succeeded')                                                      AS succeeded,
    COUNT(*) FILTER (WHERE r.status = 'failed')                                                         AS failed,
    SUM(COALESCE(r.input_tokens, 0))                                                                    AS input_tokens,
    SUM(COALESCE(r.output_tokens, 0))                                                                   AS output_tokens,
    SUM(COALESCE(r.estimated_cost_usd, 0))                                                              AS estimated_cost_usd
FROM polymarket_market_close_reviews r
WHERE r.ai_model <> ''
GROUP BY date_trunc('day', r.reviewed_at), r.ai_model;

-- 5. Market close review examples — top recent rows for ad-hoc
-- inspection panels.
CREATE OR REPLACE VIEW watchtower_market_close_review_examples_v AS
SELECT
    r.reviewed_at,
    r.condition_id,
    COALESCE(m.question, '')                                                                            AS market,
    r.verdict,
    r.confidence,
    r.admin_summary,
    r.estimated_cost_usd,
    r.ai_model,
    r.status
FROM polymarket_market_close_reviews r
LEFT JOIN polymarket_markets m ON m.id = r.market_id
WHERE r.status = 'succeeded'
ORDER BY r.reviewed_at DESC
LIMIT 200;
