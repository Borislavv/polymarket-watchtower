-- name: UpsertTrader :one
-- Insert a trader by wallet address; on conflict bump last_seen_at.
INSERT INTO polymarket_traders (wallet_address)
VALUES ($1)
ON CONFLICT (wallet_address) DO UPDATE SET
    last_seen_at = NOW()
RETURNING *;

-- name: GetTraderByWallet :one
SELECT * FROM polymarket_traders WHERE wallet_address = $1;

-- name: TraderStats :one
-- Aggregate stats for one trader since the given timestamp. Used to enrich
-- the alert payload ("this wallet's typical bet size over the last 90d").
SELECT
    COUNT(*)::bigint                       AS trade_count,
    COALESCE(SUM(notional_usd),    0)::double precision AS total_notional_usd,
    COALESCE(AVG(notional_usd),    0)::double precision AS mean_notional_usd,
    COALESCE(
        PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY notional_usd),
        0
    )::double precision                    AS median_notional_usd
FROM polymarket_trades
WHERE trader_id = $1
  AND traded_at >= $2;
