// Package walletgraph builds behavioural wallet-similarity edges
// from repeated co-trade activity (Phase A) and optional shared-
// funding edges from an external provider (Phase B).
//
// The edge math is pure-Go in computeCoTradeEdges so the worker
// can be tested without a database. Production wiring puts the
// trades repository behind CoTradeLister and the
// polymarket_wallet_graph_edges repository behind EdgeSink.
package walletgraph

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// CoTradeRow is one (wallet, event_slug, side, ts) tuple from the
// trade ingest path, restricted to a configured lookback window
// at the call site.
type CoTradeRow struct {
	Wallet    string
	EventSlug string
	Side      string // "BUY" | "SELL"
	At        time.Time
}

// Edge is the persisted wallet pair.
type Edge struct {
	WalletA         string
	WalletB         string
	Kind            string  // "co_trade" | "shared_funding" | …
	SimilarityScore float64 // 0..1
	CoEventsCount   int
	CohortID        string
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	EdgeVersion     int
}

// CoTradeLister returns rows from the trade-ingest path for one
// batch. Production reads from polymarket_trades.
type CoTradeLister interface {
	ListCoTradeRows(ctx context.Context, lookback time.Duration, limit int) ([]CoTradeRow, error)
}

// FundingProvider is optional (Phase B). Returns shared-funding
// edges. nil disables this path.
type FundingProvider interface {
	ListFundingEdges(ctx context.Context, batch int) ([]Edge, error)
}

// EdgeSink upserts edges.
type EdgeSink interface {
	UpsertEdges(ctx context.Context, edges []Edge) error
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

type Config struct {
	Enabled            bool
	Interval           time.Duration
	CoTradeWindow      time.Duration
	MinSharedEvents    int
	BatchSize          int
	EdgeVersion        int
	UseFundingProvider bool
}

// Worker drives the graph build.
type Worker struct {
	cfg     Config
	rows    CoTradeLister
	funding FundingProvider
	sink    EdgeSink
	met     *metrics.Metrics
	log     Logger
	clock   func() time.Time
	mu      sync.Mutex
}

func New(cfg Config, rows CoTradeLister, funding FundingProvider, sink EdgeSink, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:     cfg,
		rows:    rows,
		funding: funding,
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

func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.observeRun("panic")
			w.log.Error("walletgraph: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()

	if w.rows == nil || w.sink == nil {
		w.observeRun("skipped")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	rows, err := w.rows.ListCoTradeRows(ctx, w.cfg.CoTradeWindow, w.cfg.BatchSize)
	if err != nil {
		w.observeRun("failed")
		w.log.Error("walletgraph: list failed", err)
		return
	}
	edges := computeCoTradeEdges(rows, w.cfg.MinSharedEvents, w.cfg.EdgeVersion, w.clock())

	if w.cfg.UseFundingProvider && w.funding != nil {
		fundEdges, err := w.funding.ListFundingEdges(ctx, w.cfg.BatchSize)
		if err == nil {
			edges = append(edges, fundEdges...)
		} else {
			w.log.Warn("walletgraph: funding provider failed", "err", err)
		}
	}

	if len(edges) == 0 {
		w.observeRun("empty")
		return
	}
	if err := w.sink.UpsertEdges(ctx, edges); err != nil {
		w.observeRun("failed")
		w.log.Error("walletgraph: upsert failed", err)
		return
	}
	w.observeItems(len(edges), 0)
	w.observeRun("ok")
}

// computeCoTradeEdges is the pure-Go core. It counts repeated
// same-side co-entry across distinct event_slugs within the batch.
// Two wallets are an edge iff they share >= minSharedEvents event
// slugs on the same side. Similarity = shared / max(walletAEvents,
// walletBEvents) so dominating-everything wallets don't trivially
// pair with everyone.
func computeCoTradeEdges(rows []CoTradeRow, minShared int, version int, now time.Time) []Edge {
	if len(rows) == 0 || minShared < 2 {
		return nil
	}
	// (wallet, side) -> set[event_slug]
	type key struct{ wallet, side string }
	walletEvents := make(map[key]map[string]struct{})
	for _, r := range rows {
		if r.Wallet == "" || r.EventSlug == "" || r.Side == "" {
			continue
		}
		k := key{r.Wallet, r.Side}
		if walletEvents[k] == nil {
			walletEvents[k] = map[string]struct{}{}
		}
		walletEvents[k][r.EventSlug] = struct{}{}
	}
	// For deterministic output sort the keys.
	wallets := make([]key, 0, len(walletEvents))
	for k := range walletEvents {
		wallets = append(wallets, k)
	}
	sort.Slice(wallets, func(i, j int) bool {
		if wallets[i].side != wallets[j].side {
			return wallets[i].side < wallets[j].side
		}
		return wallets[i].wallet < wallets[j].wallet
	})

	var out []Edge
	for i := 0; i < len(wallets); i++ {
		a := wallets[i]
		aSet := walletEvents[a]
		for j := i + 1; j < len(wallets); j++ {
			b := wallets[j]
			if a.side != b.side {
				continue
			}
			if a.wallet == b.wallet {
				continue
			}
			shared := 0
			bSet := walletEvents[b]
			for ev := range aSet {
				if _, ok := bSet[ev]; ok {
					shared++
				}
			}
			if shared < minShared {
				continue
			}
			denom := len(aSet)
			if len(bSet) > denom {
				denom = len(bSet)
			}
			sim := float64(shared) / float64(denom)
			wA, wB := a.wallet, b.wallet
			if wA > wB {
				wA, wB = wB, wA
			}
			out = append(out, Edge{
				WalletA:         wA,
				WalletB:         wB,
				Kind:            "co_trade",
				SimilarityScore: sim,
				CoEventsCount:   shared,
				FirstSeenAt:     now,
				LastSeenAt:      now,
				EdgeVersion:     version,
			})
		}
	}
	return out
}

func (w *Worker) observeRun(status string) {
	if w.met == nil || w.met.StrategyWorkerRuns == nil {
		return
	}
	w.met.StrategyWorkerRuns.WithLabelValues("walletgraph", status).Inc()
}

func (w *Worker) observeItems(persisted, errored int) {
	if w.met == nil || w.met.StrategyWorkerItems == nil {
		return
	}
	if persisted > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("walletgraph", "persisted").Add(float64(persisted))
	}
	if errored > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("walletgraph", "errored").Add(float64(errored))
	}
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyWorkerLatency == nil {
		return
	}
	w.met.StrategyWorkerLatency.WithLabelValues("walletgraph").Observe(d.Seconds())
}
