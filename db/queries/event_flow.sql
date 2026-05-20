-- name: AggregateEventAlertsByKindAndSeverity :many
-- Returns per-(kind, severity) counts for all alerts whose market
-- has the given event_slug within the lookback window.
SELECT
    a.kind     AS kind,
    a.severity AS severity,
    COUNT(*)::bigint AS count
FROM polymarket_alerts a
JOIN polymarket_markets m ON m.id = a.market_id
WHERE m.event_slug = @event_slug
  AND a.created_at >= @since
GROUP BY a.kind, a.severity;

-- name: ListEventTopAlerts :many
-- Top N alerts (highest severity first, then newest) for one event.
SELECT
    a.id, a.kind, a.severity, a.reason, a.created_at,
    m.condition_id, m.question, m.slug AS market_slug
FROM polymarket_alerts a
JOIN polymarket_markets m ON m.id = a.market_id
WHERE m.event_slug = @event_slug
  AND a.created_at >= @since
ORDER BY
    CASE a.severity
        WHEN 'hard'     THEN 4
        WHEN 'critical' THEN 3
        WHEN 'warning'  THEN 2
        WHEN 'info'     THEN 1
        ELSE 0
    END DESC,
    a.created_at DESC
LIMIT @limit_count;

-- name: ListEventTopTrades :many
-- Top N trades by notional for one event in the lookback window.
SELECT
    t.id, t.market_id, t.outcome_token, t.side, t.price, t.notional_usd, t.traded_at,
    m.condition_id, m.question, m.slug AS market_slug,
    COALESCE(tr.wallet_address, '') AS wallet
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
LEFT JOIN polymarket_traders tr ON tr.id = t.trader_id
WHERE m.event_slug = @event_slug
  AND t.traded_at >= @since
  AND t.notional_usd >= @min_usd
ORDER BY t.notional_usd DESC, t.traded_at DESC
LIMIT @limit_count;

-- name: SumEventTradesByConditionAndSide :many
-- Per-(condition_id, outcome_token, side) sums + counts for the
-- event. Drives the strongest-side + directional-imbalance fields.
SELECT
    m.condition_id,
    t.outcome_token,
    t.side,
    SUM(t.notional_usd)::double precision AS notional_usd,
    COUNT(*)::bigint AS trade_count
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
WHERE m.event_slug = @event_slug
  AND t.traded_at >= @since
GROUP BY m.condition_id, t.outcome_token, t.side;

-- name: SumConditionTradesInWindow :many
-- Per-(outcome_token, side) sums for ONE condition_id between two
-- timestamps. Repricing pre/post-annotation flow uses this twice
-- (once for [annotation−preWindow, annotation] and once for
-- [annotation, annotation+postWindow]).
SELECT
    t.outcome_token,
    t.side,
    SUM(t.notional_usd)::double precision AS notional_usd,
    COUNT(*)::bigint AS trade_count
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
WHERE m.condition_id = @condition_id
  AND t.traded_at >= @since
  AND t.traded_at < @until
GROUP BY t.outcome_token, t.side;
