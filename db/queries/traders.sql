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
-- Per-wallet distributional summary computed server-side in a single
-- roundtrip. Powers the trader-history multiplier in the detector — a
-- $50k bet from a wallet whose typical trade is $200 is qualitatively
-- different from a $50k bet from a $1M whale, and that distinction is
-- what this query exposes. Shape mirrors BaselineDistribution so callers
-- can reuse the same readiness gates and Stats type.
--
-- $2 is the inclusive lower bound on traded_at; pass NULL to lift the
-- bound (use the trader's full stored history).
SELECT
    COUNT(*)::bigint                                                                           AS trade_count,
    COALESCE(SUM(notional_usd), 0)::double precision                                           AS total_notional_usd,
    COALESCE(AVG(notional_usd), 0)::double precision                                           AS mean_notional_usd,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY notional_usd), 0)::double precision  AS median_notional_usd,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY notional_usd), 0)::double precision  AS p95_notional_usd,
    MIN(traded_at)::timestamptz                                                                AS oldest_at,
    MAX(traded_at)::timestamptz                                                                AS newest_at
FROM polymarket_trades
WHERE trader_id = sqlc.arg(trader_id)::bigint
  AND (sqlc.narg(since)::timestamptz IS NULL OR traded_at >= sqlc.narg(since)::timestamptz);

-- name: TraderMarketSideActivity :one
-- Per-(trader, market, outcome) two-sided activity over the supplied
-- lookback. Powers the MM/arbitrage suppression filter: a wallet that
-- has been hitting BUY and SELL on the same outcome in roughly balanced
-- notional is almost certainly a market maker or arbitrageur, not an
-- informed-flow candidate, so the detector suppresses single-trade
-- alerts on that wallet/market/outcome.
--
-- $4 is the inclusive lower bound on traded_at; pass NULL to lift the
-- bound (use the trader's full stored history on this bucket).
SELECT
    COUNT(*) FILTER (WHERE side = 'BUY')::bigint                                  AS buy_count,
    COUNT(*) FILTER (WHERE side = 'SELL')::bigint                                 AS sell_count,
    COALESCE(SUM(notional_usd) FILTER (WHERE side = 'BUY'), 0)::double precision  AS buy_notional_usd,
    COALESCE(SUM(notional_usd) FILTER (WHERE side = 'SELL'), 0)::double precision AS sell_notional_usd
FROM polymarket_trades
WHERE trader_id     = sqlc.arg(trader_id)::bigint
  AND market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND (sqlc.narg(since)::timestamptz IS NULL OR traded_at >= sqlc.narg(since)::timestamptz);
