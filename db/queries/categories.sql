-- name: UpsertCategory :one
-- Insert-or-update a Polymarket category. `enabled` is preserved on update
-- (it's a local setting driven by CATEGORY_WHITELIST, not Polymarket data).
INSERT INTO polymarket_categories (external_id, slug, name, active)
VALUES ($1, $2, $3, TRUE)
ON CONFLICT (external_id) DO UPDATE SET
    slug         = EXCLUDED.slug,
    name         = EXCLUDED.name,
    active       = TRUE,
    updated_at   = NOW()
RETURNING *;

-- name: MarkCategoryEnabled :exec
-- Set the local `enabled` flag for one category. Called once per
-- CATEGORY_WHITELIST entry on app boot.
UPDATE polymarket_categories
SET enabled = $2, updated_at = NOW()
WHERE id = $1;

-- name: MarkCategoriesNotInListInactive :exec
-- Mark categories as inactive when they did not appear in the latest
-- Polymarket /tags response. `enabled` is left alone — operator intent is
-- preserved across upstream churn.
UPDATE polymarket_categories
SET active = FALSE, updated_at = NOW()
WHERE active = TRUE
  AND NOT (external_id = ANY(sqlc.arg(seen_external_ids)::text[]));

-- name: ListEnabledCategories :many
SELECT * FROM polymarket_categories
WHERE enabled = TRUE AND active = TRUE
ORDER BY id;

-- name: ListAllCategories :many
SELECT * FROM polymarket_categories ORDER BY id;

-- name: GetCategoryByExternalID :one
SELECT * FROM polymarket_categories WHERE external_id = $1;
