package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// Category is the repository-level view of a polymarket_categories row.
// Repositories return this rather than the sqlc DTO so the rest of the
// codebase doesn't import sqlc.
type Category struct {
	ID         int64
	ExternalID string
	Slug       string
	Name       string
	Enabled    bool
	Active     bool
}

// CategoryRepository owns reads and writes for polymarket_categories.
type CategoryRepository struct {
	q *sqlc.Queries
}

// NewCategoryRepository wraps a pgxpool.Pool in a CategoryRepository.
func NewCategoryRepository(pool *pgxpool.Pool) *CategoryRepository {
	return &CategoryRepository{q: sqlc.New(pool)}
}

// UpsertSeen upserts every supplied category and returns the persisted rows
// in the same order. Idempotent: a second call with the same input is a
// no-op apart from `updated_at`. `enabled` is preserved on update (see
// MarkEnabledByWhitelist for the operator-driven toggle).
func (r *CategoryRepository) UpsertSeen(ctx context.Context, cats []Category) ([]Category, error) {
	if len(cats) == 0 {
		return nil, nil
	}
	out := make([]Category, 0, len(cats))
	for _, c := range cats {
		row, err := r.q.UpsertCategory(ctx, sqlc.UpsertCategoryParams{
			ExternalID: c.ExternalID,
			Slug:       c.Slug,
			Name:       c.Name,
		})
		if err != nil {
			return nil, fmt.Errorf("upsert category %q: %w", c.ExternalID, err)
		}
		out = append(out, categoryFromSQLC(row))
	}
	return out, nil
}

// MarkSeenInactive flips `active=false` on any category not present in
// `seenExternalIDs`. Called at the tail of each discovery sweep.
func (r *CategoryRepository) MarkSeenInactive(ctx context.Context, seenExternalIDs []string) error {
	return r.q.MarkCategoriesNotInListInactive(ctx, seenExternalIDs)
}

// ApplyWhitelist sets `enabled=true` on the persisted categories whose slug
// or name matches at least one whitelist token (case-insensitive substring
// — same semantics as category.Filter). All others get `enabled=false`.
//
// Returns the categories that ended up enabled, so the caller can log the
// effective set on boot.
func (r *CategoryRepository) ApplyWhitelist(ctx context.Context, whitelist []string) ([]Category, error) {
	all, err := r.q.ListAllCategories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	tokens := normaliseTokens(whitelist)
	var enabled []Category
	for _, row := range all {
		match := tokenMatches(tokens, row.Slug, row.Name)
		if row.Enabled != match {
			if err := r.q.MarkCategoryEnabled(ctx, sqlc.MarkCategoryEnabledParams{ID: row.ID, Enabled: match}); err != nil {
				return nil, fmt.Errorf("mark category %d enabled=%v: %w", row.ID, match, err)
			}
		}
		if match {
			row.Enabled = true
			enabled = append(enabled, categoryFromSQLC(row))
		}
	}
	return enabled, nil
}

// ListEnabled returns all categories currently flagged enabled+active.
func (r *CategoryRepository) ListEnabled(ctx context.Context) ([]Category, error) {
	rows, err := r.q.ListEnabledCategories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled categories: %w", err)
	}
	out := make([]Category, 0, len(rows))
	for _, row := range rows {
		out = append(out, categoryFromSQLC(row))
	}
	return out, nil
}

func categoryFromSQLC(row sqlc.PolymarketCategories) Category {
	return Category{
		ID:         row.ID,
		ExternalID: row.ExternalID,
		Slug:       row.Slug,
		Name:       row.Name,
		Enabled:    row.Enabled,
		Active:     row.Active,
	}
}

// normaliseTokens lowercases + trims whitelist entries, dropping empties.
// Kept private to repository so the same normalisation rule lives next to
// the matcher that consumes it.
func normaliseTokens(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if n := strings.TrimSpace(strings.ToLower(t)); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// tokenMatches reports whether any token is a case-insensitive substring
// of `slug + " " + name`. Empty token set → false (no match means no
// enable, NOT "everything matches"). The category.Filter package handles
// the "empty list disables the filter" UX at a higher layer.
func tokenMatches(tokens []string, slug, name string) bool {
	if len(tokens) == 0 {
		return false
	}
	haystack := strings.ToLower(slug) + " " + strings.ToLower(name)
	for _, t := range tokens {
		if strings.Contains(haystack, t) {
			return true
		}
	}
	return false
}
