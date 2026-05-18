// Package backfill owns the BackfillWorker: a long-running background loop
// that fills polymarket_trades with the historical activity reachable
// through the Data API for every active market in a whitelisted category.
//
// Lifecycle (one market):
//
//	pending ─┬─► running ─┬─► completed         (all available history persisted)
//	         │            ├─► partial_api_limit (Data API offset-3000 cap hit)
//	         │            └─► failed            (transient upstream / DB error)
//	         ▼
//	  re-queued by ResetStaleRunning on the next tick if the process crashed.
//
// `completed` markets are no longer claimed; `partial_api_limit` markets
// can be retried later (the cap may have moved or older history may have
// become reachable through some other channel). `failed` is sticky until
// an operator inspects backfill_last_error and re-queues.
//
// Restart safety: ResetStaleRunning re-queues any `running` row older than
// the configured StaleAfter cutoff. Already-persisted trades are dedup'd
// at insert by polymarket_trades.dedup_key so re-walking from offset=0 is
// safe and cheap.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// TradeClient is the subset of dataapi.Client the worker depends on.
// Abstracted so tests can drive it without standing up an HTTP server.
type TradeClient interface {
	ListTradesPage(ctx context.Context, market vo.MarketID, offset, limit int) ([]trade.Trade, error)
}

// MarketStore is the subset of *repository.MarketRepository the worker uses.
type MarketStore interface {
	ResetStaleRunning(ctx context.Context, cutoff time.Time) error
	ListActiveForBackfill(ctx context.Context, limit int32) ([]repository.Market, error)
	BeginBackfill(ctx context.Context, marketID int64) error
	CompleteBackfill(ctx context.Context, marketID int64, status repository.BackfillStatus, oldestFetched, newestFetched time.Time) error
	FailBackfill(ctx context.Context, marketID int64, errMsg string) error
}

// TradeStore is the subset of *repository.TradeRepository.
type TradeStore interface {
	UpsertBatch(ctx context.Context, trades []repository.InsertTradeInput) (repository.UpsertResult, error)
}

// TraderStore is the subset of *repository.TraderRepository.
type TraderStore interface {
	UpsertSeen(ctx context.Context, wallets []string) ([]repository.Trader, error)
}

// Config tunes the worker.
type Config struct {
	// Interval is the tick cadence. Each tick picks up the next batch of
	// `pending`/`partial_api_limit` markets and runs a full backfill pass
	// for each. Default 1m.
	Interval time.Duration
	// BatchSize is the max number of markets claimed per tick. Lower it
	// to reduce API pressure; higher to speed up bootstrap. Default 4.
	BatchSize int
	// Concurrency caps how many markets are backfilled in parallel within
	// one tick. Bounded by the Data API rate limit; default 2.
	Concurrency int
	// PageSize is the Data API page size; clamped server-side to 500.
	PageSize int
	// StaleAfter requeues any market stuck in `running` for longer than
	// this — usually means the previous process crashed mid-backfill.
	// Default 15m.
	StaleAfter time.Duration
	// Clock is the time source; defaults to time.Now.
	Clock func() time.Time
}

// Worker is the long-running backfill loop. Safe for concurrent use across
// multiple Worker instances pointing at the same DB — BeginBackfill is
// atomic, so only one worker advances a given market.
type Worker struct {
	cfg     Config
	markets MarketStore
	trades  TradeStore
	traders TraderStore
	client  TradeClient
	log     *zerolog.Logger
	now     func() time.Time
	metrics *metrics.Metrics
}

// New wires the worker. met may be nil for tests; production wiring
// passes the shared metrics handle so BackfillPagesFetched and
// BackfillRunsTotal surface on Grafana.
func New(
	cfg Config,
	markets MarketStore,
	trades TradeStore,
	traders TraderStore,
	client TradeClient,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 4
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.PageSize <= 0 || cfg.PageSize > 500 {
		cfg.PageSize = 500
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 15 * time.Minute
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Worker{cfg: cfg, markets: markets, trades: trades, traders: traders, client: client, log: log, now: now, metrics: met}
}

// Run blocks until ctx is cancelled. Initial tick fires immediately so a
// freshly-discovered market doesn't have to wait one full interval.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick runs one backfill sweep; exposed for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	if err := w.markets.ResetStaleRunning(ctx, w.now().Add(-w.cfg.StaleAfter)); err != nil {
		w.log.Err(err).Msg("backfill: reset stale failed")
	}
	candidates, err := w.markets.ListActiveForBackfill(ctx, int32(w.cfg.BatchSize))
	if err != nil {
		w.log.Err(err).Msg("backfill: list candidates failed")
		return
	}
	if len(candidates) == 0 {
		return
	}

	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, m := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m repository.Market) {
			defer wg.Done()
			defer func() { <-sem }()
			w.backfillOne(ctx, m)
		}(m)
	}
	wg.Wait()
}

// backfillOne runs the full backfill for a single market. The transition
// pending→running is best-effort: if another worker won the race, the
// row is no longer in (`pending`, `partial_api_limit`) and BeginBackfill
// is a no-op. The next steps still run because ListActiveForBackfill
// already vetted the row.
func (w *Worker) backfillOne(ctx context.Context, m repository.Market) {
	if err := w.markets.BeginBackfill(ctx, m.ID); err != nil {
		w.log.Err(err).Int64("market_id", m.ID).Msg("backfill: begin failed")
		return
	}
	w.log.Info().
		Int64("market_id", m.ID).
		Str("condition_id", m.ConditionID).
		Msg("backfill: started")

	oldest, newest, status, finalErr := w.runPages(ctx, m)
	if finalErr != nil {
		// Context cancellation is graceful — leave the row in 'running'
		// for ResetStaleRunning to recover. Anything else is a real error.
		if errors.Is(finalErr, context.Canceled) || errors.Is(finalErr, context.DeadlineExceeded) {
			w.log.Info().Int64("market_id", m.ID).Msg("backfill: cancelled mid-run")
			return
		}
		if err := w.markets.FailBackfill(ctx, m.ID, finalErr.Error()); err != nil {
			w.log.Err(err).Int64("market_id", m.ID).Msg("backfill: fail update failed")
		}
		if w.metrics != nil {
			w.metrics.BackfillRunsTotal.WithLabelValues("failed").Inc()
		}
		w.log.Err(finalErr).Int64("market_id", m.ID).Msg("backfill: failed")
		return
	}
	if err := w.markets.CompleteBackfill(ctx, m.ID, status, oldest, newest); err != nil {
		w.log.Err(err).Int64("market_id", m.ID).Msg("backfill: complete update failed")
		return
	}
	if w.metrics != nil {
		w.metrics.BackfillRunsTotal.WithLabelValues(string(status)).Inc()
	}
	w.log.Info().
		Int64("market_id", m.ID).
		Str("status", string(status)).
		Time("oldest", oldest).
		Time("newest", newest).
		Msg("backfill: finished")
}

// runPages walks /trades pages newest→oldest until exhausted or the
// upstream offset cap is hit. Each page is persisted before advancing so
// progress is durable even if the process dies between pages.
func (w *Worker) runPages(ctx context.Context, m repository.Market) (oldest, newest time.Time, status repository.BackfillStatus, err error) {
	for offset := 0; ; offset += w.cfg.PageSize {
		if ctx.Err() != nil {
			return oldest, newest, "", ctx.Err()
		}
		page, perr := w.client.ListTradesPage(ctx, vo.MarketID(m.ConditionID), offset, w.cfg.PageSize)
		if perr != nil {
			if errors.Is(perr, dataapi.ErrOffsetCapExceeded) {
				return oldest, newest, repository.BackfillPartialAPILimit, nil
			}
			return oldest, newest, "", perr
		}
		if len(page) == 0 {
			return oldest, newest, repository.BackfillCompleted, nil
		}
		pOldest, pNewest, perr := w.persistPage(ctx, m.ID, page)
		if perr != nil {
			return oldest, newest, "", fmt.Errorf("persist page offset=%d: %w", offset, perr)
		}
		if w.metrics != nil {
			w.metrics.BackfillPagesFetched.Inc()
		}
		if oldest.IsZero() || pOldest.Before(oldest) {
			oldest = pOldest
		}
		if pNewest.After(newest) {
			newest = pNewest
		}
		if len(page) < w.cfg.PageSize {
			return oldest, newest, repository.BackfillCompleted, nil
		}
		// One more iteration may push offset past the cap; the next
		// ListTradesPage call returns ErrOffsetCapExceeded and we flip
		// to partial_api_limit there.
	}
}

// persistPage upserts traders + trades for one page. Returns the oldest
// and newest traded_at timestamps observed in the page.
func (w *Worker) persistPage(ctx context.Context, marketID int64, page []trade.Trade) (oldest, newest time.Time, err error) {
	wallets := uniqueWallets(page)
	traders, err := w.traders.UpsertSeen(ctx, wallets)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("upsert traders: %w", err)
	}
	if w.metrics != nil {
		w.metrics.TradersUpserted.Add(float64(len(traders)))
	}
	idByWallet := make(map[string]int64, len(traders))
	for _, tr := range traders {
		idByWallet[tr.WalletAddress] = tr.ID
	}

	rows := make([]repository.InsertTradeInput, 0, len(page))
	for _, t := range page {
		var traderID *int64
		if id, ok := idByWallet[t.Taker]; ok {
			traderID = &id
		}
		rows = append(rows, repository.InsertTradeInput{
			MarketID:     marketID,
			TraderID:     traderID,
			OutcomeToken: string(t.Token),
			Side:         string(t.Side),
			Price:        t.Price,
			SizeShares:   t.Size,
			NotionalUSD:  t.NotionalUSD(),
			TradedAt:     t.Timestamp,
			ExternalID:   t.ID,
			TxHash:       t.TxHash,
		})
		if oldest.IsZero() || t.Timestamp.Before(oldest) {
			oldest = t.Timestamp
		}
		if t.Timestamp.After(newest) {
			newest = t.Timestamp
		}
	}
	res, err := w.trades.UpsertBatch(ctx, rows)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("upsert trades: %w", err)
	}
	if w.metrics != nil {
		w.metrics.TradesUpserted.Add(float64(res.Inserted))
		if dup := res.Requested - res.Inserted; dup > 0 {
			w.metrics.TradesDuplicatesSkipped.Add(float64(dup))
		}
	}
	return oldest, newest, nil
}

func uniqueWallets(trades []trade.Trade) []string {
	if len(trades) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(trades))
	out := make([]string, 0, len(trades))
	for _, t := range trades {
		if t.Taker == "" {
			continue
		}
		if _, ok := seen[t.Taker]; ok {
			continue
		}
		seen[t.Taker] = struct{}{}
		out = append(out, t.Taker)
	}
	return out
}
