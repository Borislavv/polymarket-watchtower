// Package bookbars is the v11.10 producer for
// polymarket_book_feature_bars. It periodically polls Polymarket's
// public CLOB REST `/book` (or batched `/books`) for the active
// universe, computes top-N depth + spread + mid-shift features,
// and upserts one bar per (condition_id, outcome_token, bar_seconds,
// bar_start).
//
// bookvacuum reads these bars from `stagedinputs` to evaluate
// liquidity-withdrawal patterns. The worker is the ONLY producer
// for this table; the WS event path is complementary (preserves
// depth in-memory but doesn't write bars).
//
// Failure semantics: per-token failure logs + bumps the
// strategy_worker_items{op=errored} metric and never stops the
// batch. No Telegram, no AI.
package bookbars

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/clob"
)

// Candidate is one (condition, token) pair the worker should poll.
type Candidate struct {
	ConditionID string
	Token       string
}

// Bar is the persisted feature row.
type Bar struct {
	ConditionID  string
	OutcomeToken string
	BarSeconds   int
	BarStart     time.Time
	BestBid      float64
	BestAsk      float64
	MidPrice     float64
	Spread       float64
	BidDepthTopN float64
	AskDepthTopN float64
	DepthImbal   float64 // (bid - ask) / (bid + ask); -1..1
}

// CandidatesLister returns the next batch of (condition, token)
// pairs the worker should poll.
type CandidatesLister interface {
	ListBookbarsCandidates(ctx context.Context, limit int) ([]Candidate, error)
}

// BookFetcher pulls one or many books from CLOB.
type BookFetcher interface {
	GetBook(ctx context.Context, tokenID string) (clob.Book, error)
	GetBooks(ctx context.Context, tokenIDs []string) ([]clob.Book, error)
}

// Sink upserts one bar.
type Sink interface {
	UpsertBar(ctx context.Context, b Bar) error
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

// Config tunes the worker.
type Config struct {
	Enabled      bool
	Interval     time.Duration
	BarSeconds   int
	TopN         int
	MaxMarkets   int
	BatchSize    int
	FetchTimeout time.Duration
}

// Worker is the periodic CLOB book producer.
type Worker struct {
	cfg     Config
	lister  CandidatesLister
	fetcher BookFetcher
	sink    Sink
	met     *metrics.Metrics
	log     Logger
	clock   func() time.Time
	mu      sync.Mutex
}

func New(cfg Config, lister CandidatesLister, fetcher BookFetcher, sink Sink, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	if cfg.BarSeconds <= 0 {
		cfg.BarSeconds = 5
	}
	if cfg.TopN <= 0 {
		cfg.TopN = 5
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 25
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 5 * time.Second
	}
	return &Worker{
		cfg: cfg, lister: lister, fetcher: fetcher, sink: sink,
		met: met, log: log, clock: time.Now,
	}
}

func (w *Worker) WithClock(fn func() time.Time) *Worker {
	if fn != nil {
		w.clock = fn
	}
	return w
}

// Run drives the ticker; first tick is immediate.
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

// Tick polls one batch and writes bars.
func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.observeRun("panic")
			w.log.Error("bookbars: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()
	if w.lister == nil || w.fetcher == nil || w.sink == nil {
		w.observeRun("skipped")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	cands, err := w.lister.ListBookbarsCandidates(ctx, w.cfg.MaxMarkets)
	if err != nil {
		w.observeRun("failed")
		w.log.Error("bookbars: list failed", err)
		return
	}
	if len(cands) == 0 {
		w.observeRun("empty")
		return
	}
	// Pre-bucket bar_start at second granularity to make the upsert
	// idempotent within one cycle.
	barStart := w.clock().UTC().Truncate(time.Duration(w.cfg.BarSeconds) * time.Second)
	// Build token→candidate index for batch dispatch.
	tokenToCond := make(map[string]string, len(cands))
	tokens := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.Token == "" {
			continue
		}
		tokenToCond[c.Token] = c.ConditionID
		tokens = append(tokens, c.Token)
	}
	persisted, errored := 0, 0
	for offset := 0; offset < len(tokens); offset += w.cfg.BatchSize {
		end := offset + w.cfg.BatchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		batch := tokens[offset:end]
		fctx, cancel := context.WithTimeout(ctx, w.cfg.FetchTimeout)
		books, err := w.fetcher.GetBooks(fctx, batch)
		cancel()
		if err != nil {
			errored += len(batch)
			w.log.Warn("bookbars: batch failed", "size", len(batch), "err", err)
			continue
		}
		for _, b := range books {
			bar := buildBar(b, tokenToCond[b.AssetID], w.cfg.BarSeconds, w.cfg.TopN, barStart)
			if bar.OutcomeToken == "" {
				continue
			}
			if err := w.sink.UpsertBar(ctx, bar); err != nil {
				errored++
				w.log.Warn("bookbars: upsert failed", "token", b.AssetID, "err", err)
				continue
			}
			persisted++
		}
	}
	w.observeItems(persisted, errored)
	w.observeRun("ok")
}

// buildBar is pure — exposed for tests.
func buildBar(b clob.Book, conditionID string, barSeconds, topN int, barStart time.Time) Bar {
	bar := Bar{
		ConditionID:  conditionID,
		OutcomeToken: b.AssetID,
		BarSeconds:   barSeconds,
		BarStart:     barStart,
	}
	if len(b.Bids) > 0 {
		bar.BestBid = b.Bids[0].Price
		bar.BidDepthTopN = clob.TopNDepth(b.Bids, topN)
	}
	if len(b.Asks) > 0 {
		bar.BestAsk = b.Asks[0].Price
		bar.AskDepthTopN = clob.TopNDepth(b.Asks, topN)
	}
	if bar.BestBid > 0 && bar.BestAsk > 0 {
		bar.MidPrice = (bar.BestBid + bar.BestAsk) / 2
		bar.Spread = bar.BestAsk - bar.BestBid
	}
	if denom := bar.BidDepthTopN + bar.AskDepthTopN; denom > 0 {
		bar.DepthImbal = (bar.BidDepthTopN - bar.AskDepthTopN) / denom
	}
	return bar
}

// --- metric observers ---------------------------------------------

func (w *Worker) observeRun(status string) {
	if w.met == nil || w.met.StrategyWorkerRuns == nil {
		return
	}
	w.met.StrategyWorkerRuns.WithLabelValues("bookbars", status).Inc()
}

func (w *Worker) observeItems(persisted, errored int) {
	if w.met == nil || w.met.StrategyWorkerItems == nil {
		return
	}
	if persisted > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("bookbars", "persisted").Add(float64(persisted))
	}
	if errored > 0 {
		w.met.StrategyWorkerItems.WithLabelValues("bookbars", "errored").Add(float64(errored))
	}
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyWorkerLatency == nil {
		return
	}
	w.met.StrategyWorkerLatency.WithLabelValues("bookbars").Observe(d.Seconds())
}
