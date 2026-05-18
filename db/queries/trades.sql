-- name: InsertTrade :one
-- Insert a single trade. ON CONFLICT (dedup_key) DO NOTHING is the dedup
-- primitive — concurrent inserters of the same trade race to the unique
-- constraint and exactly one wins. The caller maps pgx.ErrNoRows to
-- "already existed".
INSERT INTO polymarket_trades (
    market_id, trader_id, outcome_token, side,
    price, size_shares, notional_usd,
    traded_at, external_id, tx_hash, dedup_key
)
VALUES (
    $1, $2, $3, $4,
    $5, $6, $7,
    $8, $9, $10, $11
)
ON CONFLICT (dedup_key) DO NOTHING
RETURNING *;

-- name: ListBaselineTrades :many
-- Per-(market, outcome) baseline samples within the lookback window.
-- Returned newest-first; callers compute median/mean/p95 in domain code
-- (deliberately not in SQL so the same statistics live next to the score
-- function and can be unit-tested without a database).
SELECT * FROM polymarket_trades
WHERE market_id = $1
  AND outcome_token = $2
  AND traded_at >= $3
ORDER BY traded_at DESC
LIMIT $4;

-- name: BaselineSpan :one
-- Compact summary of the per-bucket reservoir without pulling all rows.
-- Used by the readiness gate to skip listing when the baseline is plainly
-- too thin to bother with.
SELECT
    COUNT(*)::bigint                                 AS sample_count,
    COALESCE(SUM(notional_usd), 0)::double precision AS total_notional_usd,
    MIN(traded_at)::timestamptz                      AS oldest_at,
    MAX(traded_at)::timestamptz                      AS newest_at
FROM polymarket_trades
WHERE market_id = $1
  AND outcome_token = $2
  AND traded_at >= $3;

-- name: ListClusterWindowTrades :many
-- All trades in the per-category cluster window. Used by the cluster
-- detector to count distinct wallets and total notional.
SELECT t.*, m.condition_id AS market_condition_id
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
WHERE t.market_id = $1
  AND t.traded_at >= $2
ORDER BY t.traded_at DESC;

-- name: LatestTradeAt :one
-- Used by the collector to advance the per-market sync cursor without
-- keeping an in-process map. Returns NULL when no trades yet exist.
SELECT MAX(traded_at)::timestamptz AS latest_at
FROM polymarket_trades
WHERE market_id = $1;

-- name: OldestTradeAt :one
SELECT MIN(traded_at)::timestamptz AS oldest_at
FROM polymarket_trades
WHERE market_id = $1;
