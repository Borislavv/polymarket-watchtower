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

-- name: BaselineDistribution :one
-- Single-roundtrip statistical summary for the per-bucket reservoir.
-- Powers the DB-backed detector's hot path: count + total + mean + median
-- + p95 + observed time-span, server-side. PERCENTILE_CONT does the heavy
-- lifting so Go code never sorts more than this one row.
--
-- $3 is the inclusive lower bound; pass NULL (pgtype.Timestamptz{Valid:false})
-- to lift the bound (use all stored history for the bucket).
SELECT
    COUNT(*)::bigint                                                                           AS sample_count,
    COALESCE(SUM(notional_usd), 0)::double precision                                           AS total_notional_usd,
    COALESCE(AVG(notional_usd), 0)::double precision                                           AS mean_notional_usd,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY notional_usd), 0)::double precision  AS median_notional_usd,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY notional_usd), 0)::double precision  AS p95_notional_usd,
    MIN(traded_at)::timestamptz                                                                AS oldest_at,
    MAX(traded_at)::timestamptz                                                                AS newest_at
FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND (sqlc.narg(since)::timestamptz IS NULL OR traded_at >= sqlc.narg(since)::timestamptz);

-- name: ListTradesForBackfillPage :many
-- Used by the BackfillWorker to verify which fetched trade dedup_keys are
-- already persisted (defence in depth on top of ON CONFLICT DO NOTHING).
SELECT dedup_key
FROM polymarket_trades
WHERE market_id = $1
  AND dedup_key = ANY(sqlc.arg(dedup_keys)::text[]);

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

-- name: LastTradeAtBefore :one
-- Returns the most recent traded_at for a (market, outcome) STRICTLY before
-- the supplied timestamp. NULL when no prior trade exists. Powers the
-- quiet-market wake-up detector — given the current trade's timestamp it
-- yields the gap to the previous historical trade so the detector can
-- judge "idle for how long?" without re-listing rows.
SELECT MAX(traded_at)::timestamptz AS last_at
FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND traded_at     < sqlc.arg(before)::timestamptz;

-- name: AccumulationLineSummary :one
-- Server-side aggregate over one wallet's recent trades on a single
-- (market, outcome, side) bucket. Powers the same-trader accumulation
-- detector — the entire line scoring runs from this one row, with no
-- per-trade transfer. Backed by idx_trades_trader_market_outcome_side_time.
--
-- $5 is the inclusive lower bound on traded_at; pass NULL to lift the
-- bound (use the wallet's full stored history on this bucket).
--
-- avg_price is the mean of `price`. Callers convert to mean odds via
-- 1/avg_price when price > 0. max_odds is computed server-side as the
-- minimum price's inverse (price closest to 0 ⇒ largest odds).
SELECT
    COUNT(*)::bigint                                                                          AS trade_count,
    COALESCE(SUM(notional_usd), 0)::double precision                                          AS total_notional_usd,
    COALESCE(AVG(notional_usd), 0)::double precision                                          AS mean_notional_usd,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY notional_usd), 0)::double precision  AS median_notional_usd,
    COALESCE(MAX(notional_usd), 0)::double precision                                          AS max_notional_usd,
    COALESCE(MIN(notional_usd), 0)::double precision                                          AS min_notional_usd,
    COALESCE(AVG(price), 0)::double precision                                                 AS avg_price,
    COALESCE(MIN(price), 0)::double precision                                                 AS min_price,
    MIN(traded_at)::timestamptz                                                               AS oldest_at,
    MAX(traded_at)::timestamptz                                                               AS newest_at
FROM polymarket_trades
WHERE trader_id     = sqlc.arg(trader_id)::bigint
  AND market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND side          = sqlc.arg(side)::text
  AND (sqlc.narg(since)::timestamptz IS NULL OR traded_at >= sqlc.narg(since)::timestamptz);
