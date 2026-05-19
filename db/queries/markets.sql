-- name: UpsertMarket :one
-- Insert or update a market by condition_id. Backfill state fields are
-- preserved on update — only discovery-sourced fields are touched. A
-- market resurfacing after a soft-delete:
--   - active flips to TRUE
--   - deleted_at is cleared (the sanity worker would otherwise hard-purge
--     the row at retention; clearing the marker reactivates processing)
--   - purged_at is left as-is (purged markets stay purged forever; trades
--     remain queryable but the market is excluded from collect/backfill)
-- The next BackfillWorker tick picks up missing history because
-- ApplyWhitelist callers re-stamp backfill_status='pending' on resume.
INSERT INTO polymarket_markets (
    condition_id, slug, question, event_slug, event_title,
    start_date, end_date, active, closed, last_seen_at
)
VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, TRUE, $8, NOW()
)
ON CONFLICT (condition_id) DO UPDATE SET
    slug         = EXCLUDED.slug,
    question     = EXCLUDED.question,
    event_slug   = EXCLUDED.event_slug,
    event_title  = EXCLUDED.event_title,
    start_date   = EXCLUDED.start_date,
    end_date     = EXCLUDED.end_date,
    active       = TRUE,
    closed       = EXCLUDED.closed,
    last_seen_at = NOW(),
    deleted_at   = NULL,
    updated_at   = NOW()
RETURNING *;

-- name: LinkMarketCategory :exec
INSERT INTO polymarket_market_categories (market_id, category_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: UnlinkMarketCategoriesNotIn :exec
DELETE FROM polymarket_market_categories
WHERE market_id = $1
  AND NOT (category_id = ANY(sqlc.arg(keep_category_ids)::bigint[]));

-- name: UpsertMarketOutcome :exec
INSERT INTO polymarket_market_outcomes (market_id, token_id, label)
VALUES ($1, $2, $3)
ON CONFLICT (market_id, token_id) DO UPDATE SET
    label = EXCLUDED.label;

-- name: MarkMarketsInactiveNotIn :execrows
-- Mark active markets inactive AND stamp deleted_at when they did not
-- appear in the latest whitelisted-categories discovery sweep. Scoped by
-- category so markets in non-whitelisted categories are untouched.
-- The soft-delete marker (deleted_at) is set only on the active→inactive
-- transition — if a market was already inactive we leave its marker alone
-- so the sanity worker's retention window starts at the original
-- disappearance, not at every subsequent tick.
UPDATE polymarket_markets m
SET active     = FALSE,
    deleted_at = COALESCE(m.deleted_at, NOW()),
    updated_at = NOW()
WHERE m.active = TRUE
  AND NOT (m.condition_id = ANY(sqlc.arg(seen_condition_ids)::text[]))
  AND EXISTS (
      SELECT 1
      FROM polymarket_market_categories mc
      JOIN polymarket_categories c ON c.id = mc.category_id
      WHERE mc.market_id = m.id AND c.id = ANY(sqlc.arg(scope_category_ids)::bigint[])
  );

-- name: ListActiveMarketsForBackfill :many
-- Pick the next batch of markets to backfill, prioritised by upcoming
-- end_date (markets nearer resolution have the most actionable history).
-- Excludes soft-deleted and purged markets — backfill never wastes API
-- pages on a market we have no intent to monitor.
--
-- partial_api_limit cooldown: those markets have already hit the
-- documented Polymarket 3000-row offset cap. Re-running them within
-- minutes will hit the same cap and burn API quota for nothing, so
-- they only become re-claimable once their last completion is older
-- than $2 (BACKFILL_PARTIAL_RETRY_AFTER, default 6h). `pending`
-- markets bypass the cooldown — they have never been attempted.
SELECT * FROM polymarket_markets
WHERE active = TRUE
  AND deleted_at IS NULL
  AND purged_at IS NULL
  AND (
        backfill_status = 'pending'
        OR (
              backfill_status = 'partial_api_limit'
              AND (
                    backfill_completed_at IS NULL
                    OR backfill_completed_at < NOW() - sqlc.arg(partial_retry_after)::interval
                  )
            )
      )
ORDER BY end_date ASC NULLS LAST
LIMIT sqlc.arg(limit_count)::integer;

-- name: ListActiveMarketsForCollection :many
-- Active markets that have at least started backfill — collection of
-- recent trades only makes sense once we know how far back history goes.
-- Excludes soft-deleted and purged markets.
SELECT * FROM polymarket_markets
WHERE active = TRUE
  AND deleted_at IS NULL
  AND purged_at IS NULL
  AND backfill_status IN ('completed','partial_api_limit')
ORDER BY id;

-- name: BeginMarketBackfill :exec
-- Atomic transition pending|partial_api_limit → running. Idempotent: a
-- second caller picking up the same id will not flip status (status check
-- in the WHERE clause guarantees one writer wins).
UPDATE polymarket_markets
SET backfill_status     = 'running',
    backfill_started_at = NOW(),
    backfill_attempts   = backfill_attempts + 1,
    updated_at          = NOW()
WHERE id = $1
  AND backfill_status IN ('pending','partial_api_limit');

-- name: CompleteMarketBackfill :exec
UPDATE polymarket_markets
SET backfill_status            = sqlc.arg(status)::text,
    backfill_oldest_fetched_at = $2,
    backfill_newest_fetched_at = $3,
    backfill_completed_at      = NOW(),
    backfill_last_error        = NULL,
    updated_at                 = NOW()
WHERE id = $1;

-- name: FailMarketBackfill :exec
UPDATE polymarket_markets
SET backfill_status     = 'failed',
    backfill_last_error = $2,
    updated_at          = NOW()
WHERE id = $1;

-- name: ResetStaleRunningBackfills :exec
-- Recovery: a process that crashed mid-backfill leaves rows in 'running'.
-- This resets any 'running' row whose backfill_started_at is older than
-- the supplied threshold, so the next scheduler tick re-claims them.
UPDATE polymarket_markets
SET backfill_status = 'pending', updated_at = NOW()
WHERE backfill_status = 'running'
  AND backfill_started_at < $1;

-- name: GetMarketByConditionID :one
SELECT * FROM polymarket_markets WHERE condition_id = $1;

-- name: GetMarketByID :one
SELECT * FROM polymarket_markets WHERE id = $1;

-- name: ListMarketCategoryIDs :many
SELECT category_id FROM polymarket_market_categories WHERE market_id = $1;

-- name: ListSoftDeletedForPurge :many
-- Used by the sanity worker (internal/app/usecase/sanity) to find markets
-- whose soft-delete retention window has elapsed. Returns markets that:
--   - have been soft-deleted (deleted_at IS NOT NULL)
--   - are not already purged (purged_at IS NULL)
--   - crossed the retention cutoff
-- The worker then re-checks the market against the latest discover sweep;
-- a market that has resumed flips back via UpsertMarket; one that is
-- still gone gets stamped purged_at.
SELECT * FROM polymarket_markets
WHERE deleted_at IS NOT NULL
  AND deleted_at <= sqlc.arg(cutoff)::timestamptz
  AND purged_at IS NULL
ORDER BY deleted_at ASC
LIMIT sqlc.arg(claim_limit)::integer;

-- name: MarkMarketPurged :exec
-- Stamps purged_at and leaves the row intact. Trades retained for
-- analytics — the FK from polymarket_trades.market_id does not CASCADE
-- on the trade side, so a row delete would orphan trades.
UPDATE polymarket_markets
SET purged_at  = NOW(),
    active     = FALSE,
    updated_at = NOW()
WHERE id = $1
  AND purged_at IS NULL;

-- name: RequeueResumedMarket :exec
-- Called by the sanity worker (or future supervised paths) when a market
-- is resumed: clears deleted_at, flips active, resets backfill to pending
-- so the BackfillWorker picks up missing history on the next tick.
-- Discovery's UpsertMarket already handles the live-sweep case; this
-- query covers the path where the sanity worker confirms resumption via
-- a fresh DB read.
UPDATE polymarket_markets
SET active          = TRUE,
    deleted_at      = NULL,
    backfill_status = 'pending',
    updated_at      = NOW()
WHERE id = $1;

-- name: UpdateMarketCollectCursor :exec
-- Advances polymarket_markets.last_collect_traded_at to the supplied
-- timestamp, but only when it is strictly greater than the current
-- value (or the current value is NULL). Idempotent — re-running
-- against the same trade batch is a no-op. Called by persist.Sink
-- after collect's UpsertBatch; backfill never calls this.
UPDATE polymarket_markets
SET last_collect_traded_at = sqlc.arg(traded_at)::timestamptz,
    updated_at             = NOW()
WHERE id = sqlc.arg(market_id)::bigint
  AND (last_collect_traded_at IS NULL OR last_collect_traded_at < sqlc.arg(traded_at)::timestamptz);

-- name: MarketCollectCursor :one
-- Reads the per-market collect cursor. Returns NULL when the market
-- has never been touched by collect (first-sight or backfill-only),
-- which the caller maps to the BootstrapLookback default.
SELECT last_collect_traded_at
FROM polymarket_markets
WHERE id = $1;
