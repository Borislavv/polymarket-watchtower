// Package holdersync persists rank/balance snapshots of top-K
// holders per (condition_id, outcome_token).
//
// Architecture:
//   - MarketsLister hands the worker a bounded set of candidate
//     markets per tick.
//   - HoldersFetcher fetches holder rows for a single market. The
//     production adapter wraps the Polymarket Data API; tests
//     supply a fake.
//   - SnapshotSink upserts rows into polymarket_holder_snapshots.
//
// Failure semantics: per-market failure logs + bumps the
// strategy_worker_items{op=errored} metric and never stops the
// batch. Empty fetches (no holders returned) write a single zero-
// share placeholder so the freshness check downstream can see
// "we tried at T".
package holdersync

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Candidate identifies a market the worker should refresh.
type Candidate struct {
	ConditionID  string
	OutcomeToken string
	EventSlug    string
}

// HolderRow is one row in a fresh snapshot.
type HolderRow struct {
	Wallet      string
	Rank        int
	Shares      float64
	NotionalUSD float64
	PctOI       float64
	TotalOI     float64
}

// Snapshot is one (condition_id, outcome_token, snapshot_at) row set.
type Snapshot struct {
	ConditionID  string
	OutcomeToken string
	SnapshotAt   time.Time
	Rows         []HolderRow
}

// MarketsLister returns up to limit fresh-due candidates. Production
// reads from the markets repository.
type MarketsLister interface {
	ListHolderSyncCandidates(ctx context.Context, limit int, staleAfter time.Duration) ([]Candidate, error)
}

// HoldersFetcher fetches up to topK holders for one market.
type HoldersFetcher interface {
	FetchHolders(ctx context.Context, conditionID, outcomeToken string, topK int) ([]HolderRow, error)
}

// SnapshotSink persists one snapshot. Production uses sqlc-generated
// UpsertHolderSnapshot in a single transaction; tests can keep
// in-memory state.
type SnapshotSink interface {
	UpsertSnapshot(ctx context.Context, s Snapshot) error
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
	Enabled      bool
	Interval     time.Duration
	MaxMarkets   int
	TopK         int
	FetchTimeout time.Duration
	Concurrency  int
	StaleAfter   time.Duration
}

// Worker is the holdersync.Worker production scaffold.
type Worker struct {
	cfg     Config
	markets MarketsLister
	fetcher HoldersFetcher
	sink    SnapshotSink
	met     *metrics.Metrics
	log     Logger
	clock   func() time.Time
	mu      sync.Mutex
}

func New(cfg Config, markets MarketsLister, fetcher HoldersFetcher, sink SnapshotSink, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:     cfg,
		markets: markets,
		fetcher: fetcher,
		sink:    sink,
		met:     met,
		log:     log,
		clock:   time.Now,
	}
}

func (w *Worker) WithClock(fn func() time.Time) *Worker {
	if fn != nil {
		w.clock = fn
	}
	return w
}

// Run blocks until ctx cancels. Immediate first tick.
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

// Tick fetches + persists holder snapshots for one batch.
func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.observeRun("panic")
			w.log.Error("holdersync: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()

	if w.markets == nil || w.fetcher == nil || w.sink == nil {
		w.observeRun("skipped")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	cands, err := w.markets.ListHolderSyncCandidates(ctx, w.cfg.MaxMarkets, w.cfg.StaleAfter)
	if err != nil {
		w.observeRun("failed")
		w.log.Error("holdersync: list failed", err)
		return
	}
	if len(cands) == 0 {
		w.observeRun("empty")
		return
	}

	concurrency := w.cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var persisted, errored int
	var counterMu sync.Mutex

	for _, c := range cands {
		c := c
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ok := w.handleOne(ctx, c)
			counterMu.Lock()
			if ok {
				persisted++
			} else {
				errored++
			}
			counterMu.Unlock()
		}()
	}
	wg.Wait()
	w.observeItems(persisted, errored)
	w.observeRun("ok")
}

func (w *Worker) handleOne(ctx context.Context, c Candidate) bool {
	fctx := ctx
	if w.cfg.FetchTimeout > 0 {
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(ctx, w.cfg.FetchTimeout)
		defer cancel()
	}
	rows, err := w.fetcher.FetchHolders(fctx, c.ConditionID, c.OutcomeToken, w.cfg.TopK)
	if err != nil {
		w.log.Warn("holdersync: fetch failed", "condition_id", c.ConditionID, "err", err)
		return false
	}
	snap := Snapshot{
		ConditionID:  c.ConditionID,
		OutcomeToken: c.OutcomeToken,
		SnapshotAt:   w.clock(),
		Rows:         rows,
	}
	if err := w.sink.UpsertSnapshot(ctx, snap); err != nil {
		w.log.Warn("holdersync: upsert failed", "condition_id", c.ConditionID, "err", err)
		return false
	}
	return true
}

func (w *Worker) observeRun(status string) {
	if w.met == nil || w.met.StrategyWorkerRuns == nil {
		return
	}
	w.met.StrategyWorkerRuns.WithLabelValues("holdersync", status).Inc()
}

func (w *Worker) observeItems(persisted, errored int) {
	if w.met == nil || w.met.StrategyWorkerItems == nil {
		return
	}
	if persisted > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("holdersync", "persisted").Add(float64(persisted))
	}
	if errored > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("holdersync", "errored").Add(float64(errored))
	}
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyWorkerLatency == nil {
		return
	}
	w.met.StrategyWorkerLatency.WithLabelValues("holdersync").Observe(d.Seconds())
}
