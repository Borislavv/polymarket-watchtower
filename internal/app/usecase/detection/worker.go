// Package detection drains persisted trades through detect.Loop.Observe.
//
// Why this exists: before the detection queue, the only path that
// produced single-trade alerts was collect → detect.Observe inline.
// Backfill and any future source (websocket, manual import) silently
// skipped detection because they wrote straight to polymarket_trades.
// The result was a "no Info alerts overnight" failure mode whenever
// backfill outpaced collect (cursor poisoning) or backfill was the
// dominant source.
//
// Contract:
//   - Every persisted trade flows through this worker exactly once
//     (idempotent via the polymarket_trades.detected_at column).
//   - On restart, pending rows resume — the worker is stateless.
//   - The scorer's LiveAlertMaxLag gate still applies inside Observe;
//     stale trades are stamped detection_status='skipped' with reason
//     'too_old_for_live_alert' so no Telegram traffic is produced.
//   - Concurrent workers see disjoint batches via FOR UPDATE SKIP
//     LOCKED in ClaimUndetectedTrades.
package detection

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// osHostname / osPid are package-level vars so tests can override.
var (
	osHostname = func() (string, error) { return os.Hostname() }
	osPid      = func() int { return os.Getpid() }
)

// Canonical skip reasons stamped on detection_skip_reason. Used by
// metrics labels too — keep stable and short.
const (
	SkipReasonTooOldForLiveAlert = "too_old_for_live_alert"
	SkipReasonMarketUnknown      = "market_unknown"
)

// Observer is the subset of detect.Loop the worker calls. Abstracted
// so tests can plug a fake without constructing the full detect.New().
type Observer interface {
	Observe(ctx context.Context, m market.Market, t trade.Trade)
}

// TradeClaimer is the subset of repository.TradeRepository the worker
// uses. *repository.TradeRepository satisfies it.
type TradeClaimer interface {
	ClaimUndetectedTrades(ctx context.Context, workerID string, limit int32, claimTTL time.Duration) ([]repository.PendingDetectionTrade, error)
	MarkDetectionAnalyzed(ctx context.Context, tradeID int64) error
	MarkDetectionSkipped(ctx context.Context, tradeID int64, reason string) error
	MarkDetectionFailed(ctx context.Context, tradeID int64, errMsg string) error
	ResetStaleDetectionClaims(ctx context.Context, staleAfter time.Duration) (int64, error)
}

// MarketCache resolves a market condition_id to the in-memory
// market.Market. *marketcache.Cache satisfies it.
type MarketCache interface {
	Get(id vo.MarketID) (market.Market, bool)
}

// WalletResolver maps a trader_id back to the wallet address. The
// worker doesn't have the original Taker, but trader-axis lookups
// expect a wallet string. *repository.TraderRepository wrapped in a
// closure does the job.
type WalletResolver func(ctx context.Context, traderID int64) string

// Config tunes the worker. Zero values get safe defaults.
type Config struct {
	// Workers is the number of concurrent claim+process goroutines.
	Workers int
	// ClaimLimit caps the per-tick batch size per worker.
	ClaimLimit int32
	// Interval is the polling cadence between ticks.
	Interval time.Duration
	// StaleThreshold mirrors Anomaly.LiveAlertMaxLag — trades older
	// than this are stamped 'skipped' with reason
	// too_old_for_live_alert even though Observe still ran (the
	// scorer's internal gate ensures no alert was emitted).
	StaleThreshold time.Duration
	// ClaimTTL is the lease duration. A row claimed by a worker that
	// then crashes is reclaimable by another worker after this
	// timeout. Must be longer than the per-row processing budget
	// (Observe + DB write); a too-short TTL causes double-processing,
	// a too-long TTL slows crash recovery. Default 5m.
	ClaimTTL time.Duration
	// WorkerID is stamped on each leased row so an operator can see
	// who's holding what during incident diagnosis. Defaults to a
	// hostname-derived value.
	WorkerID string
	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Workers <= 0 {
		c.Workers = 16
	}
	if c.ClaimLimit <= 0 {
		c.ClaimLimit = 500
	}
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	if c.ClaimTTL <= 0 {
		c.ClaimTTL = 5 * time.Minute
	}
	if c.WorkerID == "" {
		// Hostname-derived default keeps logs interpretable on
		// multi-replica deploys. PID-suffix to disambiguate when an
		// operator restarts in place.
		hn, _ := osHostname()
		c.WorkerID = fmt.Sprintf("%s:%d", hn, osPid())
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Worker is the long-running drainer.
type Worker struct {
	cfg           Config
	claimer       TradeClaimer
	cache         MarketCache
	observer      Observer
	resolveWallet WalletResolver
	metrics       *metrics.Metrics
	log           *zerolog.Logger
}

// New constructs a Worker. claimer/cache/observer/log are required;
// resolveWallet may be nil (trader-axis silently disabled in that
// case) and metrics may be nil (no telemetry).
func New(cfg Config, claimer TradeClaimer, cache MarketCache, observer Observer, resolveWallet WalletResolver, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:           cfg,
		claimer:       claimer,
		cache:         cache,
		observer:      observer,
		resolveWallet: resolveWallet,
		metrics:       met,
		log:           log,
	}
}

// Run starts the worker until ctx is cancelled. Blocks.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick runs a single drain cycle; useful for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	// Reset stale leases first so rows abandoned by crashed siblings
	// re-enter the claimable pool. Errors are logged but don't block
	// the tick — the claim query handles the same predicate inline.
	if n, err := w.claimer.ResetStaleDetectionClaims(ctx, w.cfg.ClaimTTL); err != nil {
		w.log.Err(err).Msg("detection: reset stale claims failed")
	} else if n > 0 {
		w.log.Warn().Int64("reclaimed", n).Msg("detection: stale claims reclaimed")
	}
	var counters detectionTickCounters
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.drainLoop(ctx, &counters)
		}()
	}
	wg.Wait()

	if counters.claimed.Load() == 0 {
		w.log.Debug().Msg("detection: idle tick, queue empty")
		return
	}
	w.log.Debug().
		Int64("claimed", counters.claimed.Load()).
		Int64("analyzed", counters.analyzed.Load()).
		Int64("skipped_too_old", counters.skippedTooOld.Load()).
		Int64("skipped_market_unknown", counters.skippedMarketUnknown.Load()).
		Int64("failed_panic", counters.failedPanic.Load()).
		Int64("failed_mark", counters.failedMark.Load()).
		Msg("detection: tick summary")
}

// detectionTickCounters tallies per-tick outcomes. atomic.Int64 lets
// any worker goroutine update without coordination.
type detectionTickCounters struct {
	claimed              atomic.Int64
	analyzed             atomic.Int64
	skippedTooOld        atomic.Int64
	skippedMarketUnknown atomic.Int64
	failedPanic          atomic.Int64
	failedMark           atomic.Int64
}

// drainLoop pulls until the queue is empty or ctx cancels.
func (w *Worker) drainLoop(ctx context.Context, counters *detectionTickCounters) {
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := w.processOne(ctx, counters)
		if err != nil {
			w.log.Err(err).Msg("detection: claim failed")
			return
		}
		if n == 0 {
			return
		}
	}
}

func (w *Worker) processOne(ctx context.Context, counters *detectionTickCounters) (int, error) {
	rows, err := w.claimer.ClaimUndetectedTrades(ctx, w.cfg.WorkerID, w.cfg.ClaimLimit, w.cfg.ClaimTTL)
	if err != nil {
		if w.metrics != nil && w.metrics.DetectionFailed != nil {
			w.metrics.DetectionFailed.WithLabelValues("claim_error").Inc()
		}
		return 0, fmt.Errorf("claim undetected: %w", err)
	}
	if w.metrics != nil && w.metrics.DetectionClaimed != nil {
		w.metrics.DetectionClaimed.Add(float64(len(rows)))
	}
	if counters != nil {
		counters.claimed.Add(int64(len(rows)))
	}
	now := w.cfg.Clock()
	for _, row := range rows {
		w.handle(ctx, now, row, counters)
	}
	return len(rows), nil
}

func (w *Worker) handle(ctx context.Context, now time.Time, row repository.PendingDetectionTrade, counters *detectionTickCounters) {
	if w.metrics != nil && w.metrics.DetectionLagSeconds != nil && !row.TradedAt.IsZero() {
		w.metrics.DetectionLagSeconds.Observe(now.Sub(row.TradedAt).Seconds())
	}

	// Market must be in the in-memory cache. If discover hasn't seen
	// this market yet (rare), skip with reason market_unknown — but
	// stamp the row so we don't busy-loop on the same trade. A future
	// market re-discovery does NOT re-open the row; that's intentional:
	// a market we never knew about is a market we shouldn't alert on.
	m, ok := w.cache.Get(vo.MarketID(row.MarketConditionID))
	if !ok {
		if err := w.claimer.MarkDetectionSkipped(ctx, row.ID, SkipReasonMarketUnknown); err != nil {
			w.log.Err(err).Int64("trade_id", row.ID).Msg("detection: mark skipped failed")
		}
		w.metric("skipped", SkipReasonMarketUnknown)
		if counters != nil {
			counters.skippedMarketUnknown.Add(1)
		}
		return
	}

	// Rebuild trade.Trade from row. Wallet (Taker) is resolved via
	// the supplied callback when available — without it the trader
	// axis is silently disabled, exactly like an unknown wallet.
	t := trade.Trade{
		ID:        row.ExternalID,
		TxHash:    row.TxHash,
		Market:    m.ID,
		Token:     vo.TokenID(row.OutcomeToken),
		Side:      trade.Side(row.Side),
		Size:      row.SizeShares,
		Price:     row.Price,
		Timestamp: row.TradedAt,
	}
	if row.TraderID != nil && w.resolveWallet != nil {
		if wallet := w.resolveWallet(ctx, *row.TraderID); wallet != "" {
			t.Taker = wallet
		}
	}

	// Defence against a single bad row taking down the whole worker.
	// On panic, stamp 'failed' and continue.
	if err := w.safeObserve(ctx, m, t); err != nil {
		if markErr := w.claimer.MarkDetectionFailed(ctx, row.ID, err.Error()); markErr != nil {
			w.log.Err(markErr).Int64("trade_id", row.ID).Msg("detection: mark failed failed")
		}
		w.metric("failed", "panic")
		if w.metrics != nil && w.metrics.DetectionFailed != nil {
			w.metrics.DetectionFailed.WithLabelValues("panic").Inc()
		}
		if counters != nil {
			counters.failedPanic.Add(1)
		}
		return
	}

	// Stamp 'skipped' for stale rows so the DB tells the same truth
	// the scorer's internal lag gate enforced (no Telegram for old).
	if w.cfg.StaleThreshold > 0 && !row.TradedAt.IsZero() && now.Sub(row.TradedAt) > w.cfg.StaleThreshold {
		if err := w.claimer.MarkDetectionSkipped(ctx, row.ID, SkipReasonTooOldForLiveAlert); err != nil {
			w.log.Err(err).Int64("trade_id", row.ID).Msg("detection: mark skipped failed")
		}
		w.metric("skipped", SkipReasonTooOldForLiveAlert)
		if counters != nil {
			counters.skippedTooOld.Add(1)
		}
		return
	}
	if err := w.claimer.MarkDetectionAnalyzed(ctx, row.ID); err != nil {
		w.log.Err(err).Int64("trade_id", row.ID).Msg("detection: mark analyzed failed")
		if w.metrics != nil && w.metrics.DetectionFailed != nil {
			w.metrics.DetectionFailed.WithLabelValues("mark_analyzed").Inc()
		}
		if counters != nil {
			counters.failedMark.Add(1)
		}
		return
	}
	w.metric("analyzed", "")
	if counters != nil {
		counters.analyzed.Add(1)
	}
}

func (w *Worker) safeObserve(ctx context.Context, m market.Market, t trade.Trade) (recovered error) {
	defer func() {
		if r := recover(); r != nil {
			recovered = fmt.Errorf("panic in observe: %v", r)
		}
	}()
	w.observer.Observe(ctx, m, t)
	return nil
}

func (w *Worker) metric(status, reason string) {
	if w.metrics == nil || w.metrics.TradesAnalyzedTotal == nil {
		return
	}
	w.metrics.TradesAnalyzedTotal.WithLabelValues(status, reason).Inc()
}
