// Package thesislines is the v11.9 background matrix that
// precomputes per-(wallet, condition_id, side) directional exposure
// for the thesisaccum detector. Aggregating across linked markets
// in the detect.Loop hot path would amplify per-trade DB load with
// N+1 queries; this worker materialises the aggregate so the hot
// path reads at most `THESIS_HOTPATH_MAX_LINKED_MARKETS` bounded
// rows per evaluation.
//
// The worker is pure orchestration over three interfaces (Lister,
// Sink, Logger) so tests can inject fakes. Production wires the
// sqlc query `AggregateWalletThesisLines` + the upserter
// `UpsertWalletThesisLine`.
package thesislines

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Aggregate is one row produced by the materialiser.
type Aggregate struct {
	Wallet       string
	ConditionID  string
	EventSlug    string
	Side         string
	NotionalUSD  float64
	Trades       int
	LastTradedAt time.Time
}

// Lister returns per-(wallet, condition, side) aggregates for the
// configured lookback. Bounded by limit.
type Lister interface {
	AggregateWalletThesisLines(ctx context.Context, since time.Time, limit int) ([]Aggregate, error)
}

// Sink upserts one row.
type Sink interface {
	UpsertWalletThesisLine(ctx context.Context, a Aggregate, lookbackHours int) error
}

// Logger keeps the worker dependency-free.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, err error, kv ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, error, ...any) {}

// Config drives the worker.
type Config struct {
	Enabled    bool
	Interval   time.Duration
	Lookback   time.Duration
	MaxEvents  int
	MaxWallets int
	BatchSize  int
}

// Worker materialises the matrix on a periodic schedule.
type Worker struct {
	cfg    Config
	lister Lister
	sink   Sink
	met    *metrics.Metrics
	log    Logger
	clock  func() time.Time
	mu     sync.Mutex
}

func New(cfg Config, lister Lister, sink Sink, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 10000
	}
	return &Worker{cfg: cfg, lister: lister, sink: sink, met: met, log: log, clock: time.Now}
}

func (w *Worker) WithClock(fn func() time.Time) *Worker {
	if fn != nil {
		w.clock = fn
	}
	return w
}

// Run blocks until ctx cancels. First tick is immediate.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick materialises one batch of aggregates.
func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.observeRun("panic")
			w.log.Error("thesislines: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()
	if w.lister == nil || w.sink == nil {
		w.observeRun("skipped")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	since := w.clock().Add(-w.cfg.Lookback)
	rows, err := w.lister.AggregateWalletThesisLines(ctx, since, w.cfg.BatchSize)
	if err != nil {
		w.observeRun("failed")
		w.log.Error("thesislines: list failed", err)
		return
	}
	if len(rows) == 0 {
		w.observeRun("empty")
		return
	}
	persisted, errored := 0, 0
	lookbackHours := int(w.cfg.Lookback.Hours())
	for _, a := range rows {
		if a.Wallet == "" || a.ConditionID == "" {
			continue
		}
		if err := w.sink.UpsertWalletThesisLine(ctx, a, lookbackHours); err != nil {
			errored++
			w.log.Warn("thesislines: upsert failed", "wallet", a.Wallet, "err", err)
			continue
		}
		persisted++
	}
	w.observeItems(persisted, errored)
	w.observeRun("ok")
}

func (w *Worker) observeRun(status string) {
	if w.met == nil || w.met.StrategyWorkerRuns == nil {
		return
	}
	w.met.StrategyWorkerRuns.WithLabelValues("thesislines", status).Inc()
}

func (w *Worker) observeItems(persisted, errored int) {
	if w.met == nil || w.met.StrategyWorkerItems == nil {
		return
	}
	if persisted > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("thesislines", "persisted").Add(float64(persisted))
	}
	if errored > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("thesislines", "errored").Add(float64(errored))
	}
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyWorkerLatency == nil {
		return
	}
	w.met.StrategyWorkerLatency.WithLabelValues("thesislines").Observe(d.Seconds())
}
