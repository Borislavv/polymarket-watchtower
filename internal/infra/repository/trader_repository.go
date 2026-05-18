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

// TraderDistribution is the full server-side statistical roll-up for one
// wallet's trade history. Shape mirrors BaselineDistribution intentionally
// so the detector can flow trader stats through the same baseline.Stats
// type and reuse the same readiness gates (count, span, total).
type TraderDistribution struct {
	SampleCount       int64
	TotalNotionalUSD  float64
	MeanNotionalUSD   float64
	MedianNotionalUSD float64
	P95NotionalUSD    float64
	OldestAt          time.Time
	NewestAt          time.Time
}

// Span returns NewestAt − OldestAt. Zero when fewer than two samples exist.
func (d TraderDistribution) Span() time.Duration {
	if d.SampleCount < 2 {
		return 0
	}
	return d.NewestAt.Sub(d.OldestAt)
}

// TraderSideActivity is the two-sided BUY/SELL roll-up for one wallet on
// one (market, outcome) over a window. Powers the MM/arbitrage filter:
// near-balanced two-sided activity is the textbook signature of liquidity
// provision or arb, not informed flow.
type TraderSideActivity struct {
	BuyCount        int64
	SellCount       int64
	BuyNotionalUSD  float64
	SellNotionalUSD float64
}

// ErrTraderNotFound is returned by GetByWallet when no row matches.
var ErrTraderNotFound = errors.New("trader not found")

// TraderRepository owns reads and writes for polymarket_traders and the
// trader-scoped views over polymarket_trades.
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

// Distribution returns the full trader-history distribution over the
// supplied lookback window. Pass time.Time{} as `since` to lift the lower
// bound and aggregate over the wallet's full stored history.
func (r *TraderRepository) Distribution(ctx context.Context, traderID int64, since time.Time) (TraderDistribution, error) {
	row, err := r.q.TraderStats(ctx, sqlc.TraderStatsParams{
		TraderID: traderID,
		Since:    tsFromTime(since),
	})
	if err != nil {
		return TraderDistribution{}, fmt.Errorf("trader distribution %d: %w", traderID, err)
	}
	return TraderDistribution{
		SampleCount:       row.TradeCount,
		TotalNotionalUSD:  row.TotalNotionalUsd,
		MeanNotionalUSD:   row.MeanNotionalUsd,
		MedianNotionalUSD: row.MedianNotionalUsd,
		P95NotionalUSD:    row.P95NotionalUsd,
		OldestAt:          tsTime(row.OldestAt),
		NewestAt:          tsTime(row.NewestAt),
	}, nil
}

// SideActivity returns the wallet's two-sided BUY/SELL roll-up on one
// (market, outcome) since the supplied cutoff. Pass time.Time{} as
// `since` to lift the lower bound. Used by the MM/arbitrage filter.
func (r *TraderRepository) SideActivity(ctx context.Context, traderID, marketID int64, outcomeToken string, since time.Time) (TraderSideActivity, error) {
	row, err := r.q.TraderMarketSideActivity(ctx, sqlc.TraderMarketSideActivityParams{
		TraderID:     traderID,
		MarketID:     marketID,
		OutcomeToken: outcomeToken,
		Since:        tsFromTime(since),
	})
	if err != nil {
		return TraderSideActivity{}, fmt.Errorf("trader side activity %d/%d/%s: %w", traderID, marketID, outcomeToken, err)
	}
	return TraderSideActivity{
		BuyCount:        row.BuyCount,
		SellCount:       row.SellCount,
		BuyNotionalUSD:  row.BuyNotionalUsd,
		SellNotionalUSD: row.SellNotionalUsd,
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
