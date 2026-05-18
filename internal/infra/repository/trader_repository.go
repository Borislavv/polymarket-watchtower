package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// Trader is the repository view of polymarket_traders.
type Trader struct {
	ID            int64
	WalletAddress string
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
}

// TraderStats is the per-trader aggregate computed by SQL.
type TraderStats struct {
	TradeCount        int64
	TotalNotionalUSD  float64
	MeanNotionalUSD   float64
	MedianNotionalUSD float64
}

// ErrTraderNotFound is returned by GetByWallet when no row matches.
var ErrTraderNotFound = errors.New("trader not found")

// TraderRepository owns reads and writes for polymarket_traders.
type TraderRepository struct {
	q *sqlc.Queries
}

func NewTraderRepository(pool *pgxpool.Pool) *TraderRepository {
	return &TraderRepository{q: sqlc.New(pool)}
}

// UpsertSeen inserts each wallet that's not already known and bumps
// last_seen_at on the rest. Returns the persisted rows. Used by the
// collector/backfill workers as trades arrive.
func (r *TraderRepository) UpsertSeen(ctx context.Context, wallets []string) ([]Trader, error) {
	out := make([]Trader, 0, len(wallets))
	for _, w := range wallets {
		if w == "" {
			continue
		}
		row, err := r.q.UpsertTrader(ctx, w)
		if err != nil {
			return out, fmt.Errorf("upsert trader %q: %w", w, err)
		}
		out = append(out, traderFromSQLC(row))
	}
	return out, nil
}

// GetByWallet resolves a wallet address to a Trader, returning
// ErrTraderNotFound when the wallet has never been seen.
func (r *TraderRepository) GetByWallet(ctx context.Context, wallet string) (Trader, error) {
	row, err := r.q.GetTraderByWallet(ctx, wallet)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Trader{}, ErrTraderNotFound
		}
		return Trader{}, fmt.Errorf("get trader %q: %w", wallet, err)
	}
	return traderFromSQLC(row), nil
}

// Stats returns aggregate trade stats for one trader since `since`. The
// SQL uses PERCENTILE_CONT for the median; mean is plain AVG.
func (r *TraderRepository) Stats(ctx context.Context, traderID int64, since time.Time) (TraderStats, error) {
	id := traderID
	row, err := r.q.TraderStats(ctx, sqlc.TraderStatsParams{
		TraderID: &id,
		TradedAt: tsFromTime(since),
	})
	if err != nil {
		return TraderStats{}, fmt.Errorf("trader stats %d: %w", traderID, err)
	}
	return TraderStats{
		TradeCount:        row.TradeCount,
		TotalNotionalUSD:  row.TotalNotionalUsd,
		MeanNotionalUSD:   row.MeanNotionalUsd,
		MedianNotionalUSD: row.MedianNotionalUsd,
	}, nil
}

func traderFromSQLC(row sqlc.PolymarketTraders) Trader {
	return Trader{
		ID:            row.ID,
		WalletAddress: row.WalletAddress,
		FirstSeenAt:   row.FirstSeenAt.Time,
		LastSeenAt:    row.LastSeenAt.Time,
	}
}
