package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// Trade is the repository view of polymarket_trades. The dedup_key is
// derived inside Insert/UpsertBatch — callers do not compute it.
type Trade struct {
	ID           int64
	MarketID     int64
	TraderID     *int64
	OutcomeToken string
	Side         string
	Price        float64
	SizeShares   float64
	NotionalUSD  float64
	TradedAt     time.Time
	ExternalID   string
	TxHash       string
	IngestedAt   time.Time
	DedupKey     string
}

// InsertTradeInput is the per-row input for UpsertBatch.
type InsertTradeInput struct {
	MarketID     int64
	TraderID     *int64
	OutcomeToken string
	Side         string
	Price        float64
	SizeShares   float64
	NotionalUSD  float64
	TradedAt     time.Time
	ExternalID   string // Polymarket trade id when available
	TxHash       string
}

// BaselineQuery scopes a baseline read.
type BaselineQuery struct {
	MarketID     int64
	OutcomeToken string
	Since        time.Time // inclusive lower bound; pass time.Time{} to disable
	Limit        int32     // 0 → repository default
}

// BaselineSummary is the compact roll-up returned by SummarizeBaseline.
// Callers use this to evaluate readiness before paging samples for full
// median/mean/p95 calculation in domain code.
type BaselineSummary struct {
	SampleCount      int64
	TotalNotionalUSD float64
	OldestAt         time.Time // zero when no samples
	NewestAt         time.Time
}

// Span returns NewestAt − OldestAt. Zero when fewer than two samples exist.
func (s BaselineSummary) Span() time.Duration {
	if s.SampleCount < 2 {
		return 0
	}
	return s.NewestAt.Sub(s.OldestAt)
}

// UpsertResult is what UpsertBatch returns. We don't have a per-row
// "inserted vs already-existed" signal cheaply, so the caller gets the
// batch totals.
type UpsertResult struct {
	Requested int
	Inserted  int
}

// TradeRepository owns reads and writes for polymarket_trades.
type TradeRepository struct {
	q *sqlc.Queries
}

func NewTradeRepository(pool *pgxpool.Pool) *TradeRepository {
	return &TradeRepository{q: sqlc.New(pool)}
}

const defaultBaselineLimit = 5000

// UpsertBatch persists trades idempotently. Same trade twice → one row.
// Inserted counts new rows; Requested counts attempts (including no-ops
// where the dedup_key already existed).
func (r *TradeRepository) UpsertBatch(ctx context.Context, trades []InsertTradeInput) (UpsertResult, error) {
	out := UpsertResult{Requested: len(trades)}
	for _, t := range trades {
		key := DedupKeyForTrade(t)
		_, err := r.q.InsertTrade(ctx, sqlc.InsertTradeParams{
			MarketID:     t.MarketID,
			TraderID:     t.TraderID,
			OutcomeToken: t.OutcomeToken,
			Side:         t.Side,
			Price:        t.Price,
			SizeShares:   t.SizeShares,
			NotionalUsd:  t.NotionalUSD,
			TradedAt:     tsFromTime(t.TradedAt),
			ExternalID:   strPtr(t.ExternalID),
			TxHash:       strPtr(t.TxHash),
			DedupKey:     key,
		})
		switch {
		case err == nil:
			out.Inserted++
		case errors.Is(err, pgx.ErrNoRows):
			// ON CONFLICT DO NOTHING fired; row already existed.
		default:
			return out, fmt.Errorf("insert trade %q: %w", key, err)
		}
	}
	return out, nil
}

// ListBaseline returns up to Limit recent samples for the bucket. Used by
// the detector to compute median/p95 in domain code.
func (r *TradeRepository) ListBaseline(ctx context.Context, q BaselineQuery) ([]Trade, error) {
	if q.Limit <= 0 {
		q.Limit = defaultBaselineLimit
	}
	rows, err := r.q.ListBaselineTrades(ctx, sqlc.ListBaselineTradesParams{
		MarketID:     q.MarketID,
		OutcomeToken: q.OutcomeToken,
		TradedAt:     tsFromTime(q.Since),
		Limit:        q.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list baseline trades: %w", err)
	}
	out := make([]Trade, 0, len(rows))
	for _, row := range rows {
		out = append(out, tradeFromSQLC(row))
	}
	return out, nil
}

// SummarizeBaseline returns the compact summary used by the readiness gate.
func (r *TradeRepository) SummarizeBaseline(ctx context.Context, q BaselineQuery) (BaselineSummary, error) {
	row, err := r.q.BaselineSpan(ctx, sqlc.BaselineSpanParams{
		MarketID:     q.MarketID,
		OutcomeToken: q.OutcomeToken,
		TradedAt:     tsFromTime(q.Since),
	})
	if err != nil {
		return BaselineSummary{}, fmt.Errorf("baseline span: %w", err)
	}
	return BaselineSummary{
		SampleCount:      row.SampleCount,
		TotalNotionalUSD: row.TotalNotionalUsd,
		OldestAt:         tsTime(row.OldestAt),
		NewestAt:         tsTime(row.NewestAt),
	}, nil
}

// LatestTradedAt returns the newest traded_at for the market, or the zero
// time when no trades exist yet. Used by the collector to advance its
// sync cursor without keeping an in-process map.
func (r *TradeRepository) LatestTradedAt(ctx context.Context, marketID int64) (time.Time, error) {
	row, err := r.q.LatestTradeAt(ctx, marketID)
	if err != nil {
		return time.Time{}, fmt.Errorf("latest trade at: %w", err)
	}
	return tsTime(row), nil
}

// OldestTradedAt is the symmetric helper used by the backfill worker.
func (r *TradeRepository) OldestTradedAt(ctx context.Context, marketID int64) (time.Time, error) {
	row, err := r.q.OldestTradeAt(ctx, marketID)
	if err != nil {
		return time.Time{}, fmt.Errorf("oldest trade at: %w", err)
	}
	return tsTime(row), nil
}

func tradeFromSQLC(row sqlc.PolymarketTrades) Trade {
	return Trade{
		ID:           row.ID,
		MarketID:     row.MarketID,
		TraderID:     row.TraderID,
		OutcomeToken: row.OutcomeToken,
		Side:         row.Side,
		Price:        row.Price,
		SizeShares:   row.SizeShares,
		NotionalUSD:  row.NotionalUsd,
		TradedAt:     row.TradedAt.Time,
		ExternalID:   derefStr(row.ExternalID),
		TxHash:       derefStr(row.TxHash),
		IngestedAt:   row.IngestedAt.Time,
		DedupKey:     row.DedupKey,
	}
}

// DedupKeyForTrade computes the dedup key for a trade. Prefers
// ExternalID when present (Polymarket's `id` field is a content hash so
// it survives re-ingest); otherwise falls back to a stable SHA-256 over
// the composite. The same inputs always produce the same key — concurrent
// inserters race to the unique constraint, one wins.
func DedupKeyForTrade(t InsertTradeInput) string {
	if t.ExternalID != "" {
		return "ext:" + t.ExternalID
	}
	// Stable composite. ts is unix-nanos to disambiguate two trades from
	// the same wallet at the same human-second on the same market.
	composite := strconv.FormatInt(t.MarketID, 10) + "|" +
		t.OutcomeToken + "|" +
		traderKey(t.TraderID) + "|" +
		strconv.FormatInt(t.TradedAt.UnixNano(), 10) + "|" +
		strconv.FormatFloat(t.Price, 'f', -1, 64) + "|" +
		strconv.FormatFloat(t.SizeShares, 'f', -1, 64) + "|" +
		t.Side
	sum := sha256.Sum256([]byte(composite))
	return "sha:" + hex.EncodeToString(sum[:])
}

func traderKey(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}
