package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// BackfillStatus is the typed string used by polymarket_markets.backfill_status.
// Matches the CHECK constraint in the migration.
type BackfillStatus string

const (
	BackfillPending         BackfillStatus = "pending"
	BackfillRunning         BackfillStatus = "running"
	BackfillCompleted       BackfillStatus = "completed"
	BackfillPartialAPILimit BackfillStatus = "partial_api_limit"
	BackfillFailed          BackfillStatus = "failed"
	BackfillSkipped         BackfillStatus = "skipped"
)

// Market is the repository-level view of polymarket_markets.
type Market struct {
	ID                      int64
	ConditionID             string
	Slug                    string
	Question                string
	EventSlug               string
	EventTitle              string
	StartDate               time.Time
	EndDate                 time.Time
	Active                  bool
	Closed                  bool
	LastSeenAt              time.Time
	BackfillStatus          BackfillStatus
	BackfillOldestFetchedAt time.Time
	BackfillNewestFetchedAt time.Time
	BackfillAttempts        int32
	BackfillLastError       string
	BackfillStartedAt       time.Time
	BackfillCompletedAt     time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// UpsertMarketInput is what the discovery worker hands to UpsertSeen. It
// is deliberately a subset of the full Market — only discovery-sourced
// fields. Backfill state is owned by the BackfillWorker.
type UpsertMarketInput struct {
	ConditionID string
	Slug        string
	Question    string
	EventSlug   string
	EventTitle  string
	StartDate   time.Time
	EndDate     time.Time
	Closed      bool
	CategoryIDs []int64 // links to polymarket_categories.id
}

// MarketRepository owns reads and writes for markets, market↔category
// links, and the per-market backfill state.
type MarketRepository struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewMarketRepository(pool *pgxpool.Pool) *MarketRepository {
	return &MarketRepository{pool: pool, q: sqlc.New(pool)}
}

// UpsertSeen upserts each market, refreshes its category links, and returns
// the persisted rows in input order. Each market is processed in its own
// short transaction so a single bad row doesn't roll back the batch.
func (r *MarketRepository) UpsertSeen(ctx context.Context, markets []UpsertMarketInput) ([]Market, error) {
	out := make([]Market, 0, len(markets))
	for _, m := range markets {
		row, err := r.upsertOne(ctx, m)
		if err != nil {
			return out, fmt.Errorf("upsert market %q: %w", m.ConditionID, err)
		}
		out = append(out, row)
	}
	return out, nil
}

func (r *MarketRepository) upsertOne(ctx context.Context, m UpsertMarketInput) (Market, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Market{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.q.WithTx(tx)

	row, err := q.UpsertMarket(ctx, sqlc.UpsertMarketParams{
		ConditionID: m.ConditionID,
		Slug:        m.Slug,
		Question:    m.Question,
		EventSlug:   strPtr(m.EventSlug),
		EventTitle:  strPtr(m.EventTitle),
		StartDate:   tsFromTime(m.StartDate),
		EndDate:     tsFromTime(m.EndDate),
		Closed:      m.Closed,
	})
	if err != nil {
		return Market{}, fmt.Errorf("upsert: %w", err)
	}
	for _, catID := range m.CategoryIDs {
		if err := q.LinkMarketCategory(ctx, sqlc.LinkMarketCategoryParams{
			MarketID:   row.ID,
			CategoryID: catID,
		}); err != nil {
			return Market{}, fmt.Errorf("link category %d: %w", catID, err)
		}
	}
	if err := q.UnlinkMarketCategoriesNotIn(ctx, sqlc.UnlinkMarketCategoriesNotInParams{
		MarketID:        row.ID,
		KeepCategoryIds: m.CategoryIDs,
	}); err != nil {
		return Market{}, fmt.Errorf("unlink stale categories: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Market{}, fmt.Errorf("commit: %w", err)
	}
	return marketFromSQLC(row), nil
}

// MarkSeenInactive flips `active=false` on markets within the supplied
// category scope that did NOT appear in the latest sweep. Scoping by
// category prevents the worker from inadvertently marking markets in
// non-whitelisted categories as inactive (they were never in the sweep
// to begin with).
func (r *MarketRepository) MarkSeenInactive(ctx context.Context, seenConditionIDs []string, scopeCategoryIDs []int64) error {
	if len(scopeCategoryIDs) == 0 {
		return nil
	}
	return r.q.MarkMarketsInactiveNotIn(ctx, sqlc.MarkMarketsInactiveNotInParams{
		SeenConditionIds: seenConditionIDs,
		ScopeCategoryIds: scopeCategoryIDs,
	})
}

// ListActiveForBackfill returns the next batch of markets to backfill,
// ordered by upcoming end_date ASC (nearer-to-resolution first).
func (r *MarketRepository) ListActiveForBackfill(ctx context.Context, limit int32) ([]Market, error) {
	rows, err := r.q.ListActiveMarketsForBackfill(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list backfill candidates: %w", err)
	}
	return marketsFromSQLC(rows), nil
}

// ListActiveForCollection returns markets eligible for recent-trade pulls
// (i.e. backfill is at least partially done).
func (r *MarketRepository) ListActiveForCollection(ctx context.Context) ([]Market, error) {
	rows, err := r.q.ListActiveMarketsForCollection(ctx)
	if err != nil {
		return nil, fmt.Errorf("list collection candidates: %w", err)
	}
	return marketsFromSQLC(rows), nil
}

// BeginBackfill atomically transitions pending|partial_api_limit → running.
// Returns true when this caller won the transition; false when the row
// was already running or in some other state.
func (r *MarketRepository) BeginBackfill(ctx context.Context, marketID int64) error {
	return r.q.BeginMarketBackfill(ctx, marketID)
}

// CompleteBackfill marks the run done and records the observed window.
// `status` should be one of: completed, partial_api_limit, skipped.
func (r *MarketRepository) CompleteBackfill(ctx context.Context, marketID int64, status BackfillStatus, oldestFetched, newestFetched time.Time) error {
	return r.q.CompleteMarketBackfill(ctx, sqlc.CompleteMarketBackfillParams{
		ID:                      marketID,
		BackfillOldestFetchedAt: tsFromTime(oldestFetched),
		BackfillNewestFetchedAt: tsFromTime(newestFetched),
		Status:                  string(status),
	})
}

// FailBackfill records the error and leaves the row in 'failed'.
func (r *MarketRepository) FailBackfill(ctx context.Context, marketID int64, errMsg string) error {
	return r.q.FailMarketBackfill(ctx, sqlc.FailMarketBackfillParams{
		ID:                marketID,
		BackfillLastError: strPtr(errMsg),
	})
}

// ResetStaleRunning re-queues any market in 'running' since before the
// supplied cutoff. Called by the BackfillWorker on each tick to recover
// from a crashed process.
func (r *MarketRepository) ResetStaleRunning(ctx context.Context, cutoff time.Time) error {
	return r.q.ResetStaleRunningBackfills(ctx, tsFromTime(cutoff))
}

// GetByConditionID is used by the collector to resolve the local market
// id from an upstream trade payload.
func (r *MarketRepository) GetByConditionID(ctx context.Context, conditionID string) (Market, error) {
	row, err := r.q.GetMarketByConditionID(ctx, conditionID)
	if err != nil {
		return Market{}, fmt.Errorf("get market %q: %w", conditionID, err)
	}
	return marketFromSQLC(row), nil
}

// UpsertOutcome links a market outcome (Yes/No/etc.) to its CLOB token id.
func (r *MarketRepository) UpsertOutcome(ctx context.Context, marketID int64, tokenID, label string) error {
	return r.q.UpsertMarketOutcome(ctx, sqlc.UpsertMarketOutcomeParams{
		MarketID: marketID,
		TokenID:  tokenID,
		Label:    label,
	})
}

func marketsFromSQLC(rows []sqlc.PolymarketMarkets) []Market {
	out := make([]Market, 0, len(rows))
	for _, r := range rows {
		out = append(out, marketFromSQLC(r))
	}
	return out
}

func marketFromSQLC(row sqlc.PolymarketMarkets) Market {
	return Market{
		ID:                      row.ID,
		ConditionID:             row.ConditionID,
		Slug:                    row.Slug,
		Question:                row.Question,
		EventSlug:               derefStr(row.EventSlug),
		EventTitle:              derefStr(row.EventTitle),
		StartDate:               tsTime(row.StartDate),
		EndDate:                 tsTime(row.EndDate),
		Active:                  row.Active,
		Closed:                  row.Closed,
		LastSeenAt:              row.LastSeenAt.Time,
		BackfillStatus:          BackfillStatus(row.BackfillStatus),
		BackfillOldestFetchedAt: tsTime(row.BackfillOldestFetchedAt),
		BackfillNewestFetchedAt: tsTime(row.BackfillNewestFetchedAt),
		BackfillAttempts:        row.BackfillAttempts,
		BackfillLastError:       derefStr(row.BackfillLastError),
		BackfillStartedAt:       tsTime(row.BackfillStartedAt),
		BackfillCompletedAt:     tsTime(row.BackfillCompletedAt),
		CreatedAt:               row.CreatedAt.Time,
		UpdatedAt:               row.UpdatedAt.Time,
	}
}
