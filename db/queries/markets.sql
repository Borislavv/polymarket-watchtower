-- name: UpsertMarket :one
-- Insert or update a market by condition_id. Backfill state fields are
-- preserved on update — only discovery-sourced fields are touched. A
-- previously-inactive market that reappears flips `active=TRUE` and the
-- next BackfillWorker tick will pick up missing history.
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

-- name: MarkMarketsInactiveNotIn :exec
-- Mark active markets inactive when they did not appear in the latest
-- whitelisted-categories discovery sweep. Scoped by category to avoid
-- penalising markets in categories we don't currently sync.
UPDATE polymarket_markets m
SET active = FALSE, updated_at = NOW()
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
-- Limit is the per-tick claim count.
SELECT * FROM polymarket_markets
WHERE active = TRUE
  AND backfill_status IN ('pending','partial_api_limit')
ORDER BY end_date ASC NULLS LAST
LIMIT $1;

-- name: ListActiveMarketsForCollection :many
-- Active markets that have at least started backfill — collection of
-- recent trades only makes sense once we know how far back history goes.
SELECT * FROM polymarket_markets
WHERE active = TRUE
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
