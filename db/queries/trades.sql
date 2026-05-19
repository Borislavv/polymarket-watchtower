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
    COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY notional_usd), 0)::double precision  AS p99_notional_usd,
    MIN(traded_at)::timestamptz                                                                AS oldest_at,
    MAX(traded_at)::timestamptz                                                                AS newest_at
FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND (sqlc.narg(since)::timestamptz IS NULL OR traded_at >= sqlc.narg(since)::timestamptz);

-- name: OwnershipShares :one
-- Server-side aggregate of (wallet, market, outcome) share-count flow
-- vs the outcome's total BUY-side flow. Powers the trade-flow
-- approximation of market-ownership concentration.
--
-- IMPORTANT: this is an APPROXIMATION, not a holders read. The CLOB
-- API holders endpoint is not wired upstream. `wallet_buy_shares` and
-- `wallet_sell_shares` are summed only over trades the watchtower
-- ingested; a wallet that transferred shares off-chain or sold to a
-- counterparty whose trade we didn't observe is invisible to this
-- query. Treat the percentage as directional, not authoritative.
SELECT
    COALESCE(SUM(size_shares) FILTER (WHERE side = 'BUY'  AND trader_id = sqlc.arg(trader_id)::bigint), 0)::double precision AS wallet_buy_shares,
    COALESCE(SUM(size_shares) FILTER (WHERE side = 'SELL' AND trader_id = sqlc.arg(trader_id)::bigint), 0)::double precision AS wallet_sell_shares,
    COALESCE(SUM(size_shares) FILTER (WHERE side = 'BUY'),                                              0)::double precision AS market_buy_shares
FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text;

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

-- name: ClaimUndetectedTrades :many
-- Atomically lease a batch of trades that need detection processing.
-- The UPDATE flips detection_claimed_at to NOW() and stamps the
-- worker id; the inner SELECT … FOR UPDATE SKIP LOCKED ensures
-- concurrent workers see disjoint batches. A leased row stays in
-- 'pending' (detected_at IS NULL) until the worker calls
-- MarkTradeDetectionResult; if the worker crashes, ResetStaleDetectionClaims
-- reclaims rows whose lease is older than the configured TTL.
UPDATE polymarket_trades t
SET detection_claimed_at = NOW(),
    detection_worker_id  = sqlc.arg(worker_id)::text
FROM (
    SELECT id
    FROM polymarket_trades
    WHERE detected_at IS NULL
      AND (detection_claimed_at IS NULL OR detection_claimed_at < NOW() - sqlc.arg(claim_ttl)::interval)
    ORDER BY traded_at DESC, id DESC
    LIMIT sqlc.arg(claim_limit)::integer
    FOR UPDATE SKIP LOCKED
) AS picked
JOIN polymarket_markets m ON TRUE
WHERE t.id = picked.id AND m.id = t.market_id
RETURNING t.id, t.market_id, t.trader_id, t.outcome_token, t.side, t.price,
          t.size_shares, t.notional_usd, t.traded_at, t.external_id,
          t.tx_hash, t.ingested_at, t.dedup_key,
          t.detection_attempts,
          m.condition_id AS market_condition_id;

-- name: ResetStaleDetectionClaims :execrows
-- Free leases held longer than the supplied interval, so the next
-- claim picks the row back up. Returns the number of rows reset (the
-- worker logs this so an operator can spot crash loops).
UPDATE polymarket_trades
SET detection_claimed_at = NULL,
    detection_worker_id  = NULL
WHERE detected_at IS NULL
  AND detection_claimed_at IS NOT NULL
  AND detection_claimed_at < NOW() - sqlc.arg(stale_after)::interval;

-- name: MarkTradeDetectionResult :exec
-- Stamp the terminal state for one trade. detected_at = NOW() always.
-- detection_status is one of 'analyzed' | 'skipped' | 'failed' (the
-- CHECK constraint enforces this). detection_skip_reason is set only
-- when status='skipped'; last_detection_error only when status='failed'.
-- The lease columns are cleared atomically so a row can never enter a
-- "completed but still leased" state.
UPDATE polymarket_trades
SET detected_at           = NOW(),
    detection_status      = sqlc.arg(status)::text,
    detection_skip_reason = sqlc.narg(skip_reason)::text,
    last_detection_error  = sqlc.narg(error_message)::text,
    detection_attempts    = detection_attempts + 1,
    detection_claimed_at  = NULL,
    detection_worker_id   = NULL
WHERE id = sqlc.arg(trade_id)::bigint;

-- name: PendingDetectionCount :one
-- Diagnostic: how many trades are still pending detection. Used by
-- /stats and Grafana so operators can see whether the worker is
-- keeping up.
SELECT COUNT(*)::bigint AS pending FROM polymarket_trades WHERE detected_at IS NULL;

-- name: DetectionStatusBreakdown :many
-- /stats break-down: rows by terminal detection state. NULL status is
-- reported as 'pending'.
SELECT COALESCE(detection_status, 'pending')::text AS status,
       COUNT(*)::bigint                            AS count
FROM polymarket_trades
GROUP BY 1
ORDER BY 1;

-- name: TraderFirstSeenAt :one
-- Earliest persisted trade timestamp for a wallet's full history.
-- Used by the new-wallet / dormant-wallet context boosters; the
-- detection worker calls this when stamping context on a Finding.
SELECT MIN(t.traded_at)::timestamptz AS first_seen_at
FROM polymarket_trades t
WHERE t.trader_id = sqlc.arg(trader_id)::bigint;

-- name: TraderLastSeenBefore :one
-- Most-recent trade timestamp STRICTLY before the supplied cutoff.
-- Used by the dormant-wallet booster to ask "how long has this wallet
-- been idle just before this trade?".
SELECT MAX(t.traded_at)::timestamptz AS last_at
FROM polymarket_trades t
WHERE t.trader_id = sqlc.arg(trader_id)::bigint
  AND t.traded_at < sqlc.arg(before)::timestamptz;

-- name: PriceWindowStats :one
-- Per-(market, outcome) price/volume stats over a lookback window.
-- Powers the late-market stable-favorite worker — stability,
-- adverse-drift, and liquidity gates all read from this one row.
--
-- first/last via array_agg(ORDER BY) so a single scan yields both.
-- STDDEV_POP (not _SAMP) because we treat the window as the
-- population for stability purposes; the worker enforces a minimum
-- sample count upstream.
SELECT
    COUNT(*)::bigint                                            AS sample_count,
    COALESCE(AVG(price), 0)::double precision                   AS mean_price,
    COALESCE(STDDEV_POP(price), 0)::double precision            AS stddev_price,
    COALESCE(MIN(price), 0)::double precision                   AS min_price,
    COALESCE(MAX(price), 0)::double precision                   AS max_price,
    COALESCE((array_agg(price ORDER BY traded_at ASC))[1],  0)::double precision AS first_price,
    COALESCE((array_agg(price ORDER BY traded_at DESC))[1], 0)::double precision AS last_price,
    COALESCE(SUM(notional_usd), 0)::double precision            AS volume_usd,
    COALESCE(SUM(notional_usd) FILTER (WHERE side = 'BUY'),  0)::double precision AS buy_volume_usd,
    COALESCE(SUM(notional_usd) FILTER (WHERE side = 'SELL'), 0)::double precision AS sell_volume_usd
FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
  AND traded_at    >= sqlc.arg(since)::timestamptz;

-- name: ListLateMarketCandidates :many
-- Active markets whose lifecycle progress has crossed the supplied
-- threshold AND haven't been soft-deleted/purged. Returns enough
-- context for the stable-favorite worker to build inputs without a
-- second roundtrip per row.
--
-- The lifecycle math mirrors the in-memory market.LifecyclePct used
-- by the per-trade scorer so the two strategies agree on what
-- "late-market" means.
SELECT m.id, m.condition_id, m.slug, m.question, m.event_slug, m.start_date, m.end_date,
       (100.0 * EXTRACT(EPOCH FROM (NOW() - m.start_date)) /
              NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0))::double precision AS lifecycle_pct
FROM polymarket_markets m
WHERE m.active = TRUE
  AND m.deleted_at IS NULL
  AND m.purged_at  IS NULL
  AND m.start_date IS NOT NULL
  AND m.end_date   IS NOT NULL
  AND m.end_date    > NOW()
  AND (100.0 * EXTRACT(EPOCH FROM (NOW() - m.start_date)) /
             NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0))::double precision >= sqlc.arg(min_lifecycle_pct)::double precision
ORDER BY lifecycle_pct DESC
LIMIT sqlc.arg(limit_count)::integer;

-- name: LatestPriceForOutcome :one
-- Most recent observed price for (market, outcome). Used by the
-- stable-favorite worker as "current price" since there is no CLOB
-- bid/ask wired upstream — the most recent trade is the best proxy
-- we have for the implied probability.
SELECT price FROM polymarket_trades
WHERE market_id     = sqlc.arg(market_id)::bigint
  AND outcome_token = sqlc.arg(outcome_token)::text
ORDER BY traded_at DESC
LIMIT 1;
