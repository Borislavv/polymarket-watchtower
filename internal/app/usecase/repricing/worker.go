// Package repricing owns the open/close lifecycle of
// polymarket_repricing_windows rows. The window represents a
// "we expect the market to move after this annotation / catalyst"
// observation horizon; the repricinglag detector consumes the
// closed rows.
//
// Architecture:
//   - TriggerLister surfaces freshly seen annotations / catalysts
//     that don't yet have an open window.
//   - PriceSampler reads current and peer prices.
//   - WindowSink upserts / closes window rows.
//
// All deps are interfaces so tests use fakes; production wires
// against eventpagecontext + eventcatalyst + the trade repository.
package repricing

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TriggerKind classifies the window's origin.
type TriggerKind string

const (
	TriggerAnnotation TriggerKind = "annotation"
	TriggerCatalyst   TriggerKind = "catalyst"
)

// Trigger describes one fresh observation that wants a window.
type Trigger struct {
	ConditionID       string
	EventSlug         string
	Kind              TriggerKind
	Ref               string // annotation hash / catalyst id
	OpenedAt          time.Time
	ExpectedImpactMin float64
	ExpectedImpactMax float64
	SideBias          string
	BaselinePrice     float64
}

// OpenWindow is one already-open row that may be ready to close.
type OpenWindow struct {
	ID            int64
	ConditionID   string
	EventSlug     string
	OpenedAt      time.Time
	ClosesAt      time.Time
	BaselinePrice float64
	SideBias      string
}

// TriggerLister returns fresh-trigger payloads.
type TriggerLister interface {
	ListNewTriggers(ctx context.Context, lookback time.Duration, limit int) ([]Trigger, error)
}

// OpenLister returns open windows that may be ready to close.
type OpenLister interface {
	ListOpenWindows(ctx context.Context, dueBefore time.Time, limit int) ([]OpenWindow, error)
}

// PriceSampler exposes target + peer price reads.
type PriceSampler interface {
	SampleTarget(ctx context.Context, conditionID string) (price float64, ok bool, err error)
	SamplePeerMedian(ctx context.Context, eventSlug, anchorConditionID string) (median float64, n int, err error)
}

// WindowSink persists windows.
type WindowSink interface {
	OpenWindow(ctx context.Context, t Trigger, closesAt time.Time) error
	CloseWindow(ctx context.Context, id int64, observed, peer, lag float64, status string) error
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
	Enabled        bool
	Interval       time.Duration
	OpenLookback   time.Duration
	MaxOpenWindows int
	CloseAfter     time.Duration
}

// Worker drives the open/close cycle.
type Worker struct {
	cfg      Config
	triggers TriggerLister
	open     OpenLister
	sampler  PriceSampler
	sink     WindowSink
	met      *metrics.Metrics
	log      Logger
	clock    func() time.Time
	mu       sync.Mutex
}

func New(cfg Config, triggers TriggerLister, open OpenLister, sampler PriceSampler, sink WindowSink, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:      cfg,
		triggers: triggers,
		open:     open,
		sampler:  sampler,
		sink:     sink,
		met:      met,
		log:      log,
		clock:    time.Now,
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

// Tick opens + closes pending windows.
func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.observeRun("panic")
			w.log.Error("repricing: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()

	if w.triggers == nil || w.open == nil || w.sampler == nil || w.sink == nil {
		w.observeRun("skipped")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.clock()
	opened := w.openPhase(ctx, now)
	closed := w.closePhase(ctx, now)
	w.observeItems(opened, closed, 0)
	w.observeRun("ok")
}

func (w *Worker) openPhase(ctx context.Context, now time.Time) int {
	triggers, err := w.triggers.ListNewTriggers(ctx, w.cfg.OpenLookback, w.cfg.MaxOpenWindows)
	if err != nil {
		w.log.Warn("repricing: list triggers failed", "err", err)
		return 0
	}
	n := 0
	for _, t := range triggers {
		closesAt := t.OpenedAt.Add(w.cfg.CloseAfter)
		if closesAt.Before(now) {
			// Trigger already aged out; record-then-close immediately.
			closesAt = now
		}
		if err := w.sink.OpenWindow(ctx, t, closesAt); err != nil {
			w.log.Warn("repricing: open window failed",
				"condition_id", t.ConditionID, "err", err)
			continue
		}
		n++
	}
	return n
}

func (w *Worker) closePhase(ctx context.Context, now time.Time) int {
	open, err := w.open.ListOpenWindows(ctx, now, w.cfg.MaxOpenWindows)
	if err != nil {
		w.log.Warn("repricing: list open windows failed", "err", err)
		return 0
	}
	n := 0
	for _, win := range open {
		price, ok, err := w.sampler.SampleTarget(ctx, win.ConditionID)
		if err != nil || !ok {
			// v11.9: distinguish "no target price" (stale) from
			// "blocked by high ambiguity" (closed_blocked). The
			// sampler returns ok=false for both database miss and
			// stale conditions — we record stale_missing_price.
			if err := w.sink.CloseWindow(ctx, win.ID, 0, 0, 0, "stale_missing_price"); err != nil {
				w.log.Warn("repricing: close stale failed", "id", win.ID, "err", err)
				continue
			}
			n++
			continue
		}
		observed := price - win.BaselinePrice
		peer, peerN, _ := w.sampler.SamplePeerMedian(ctx, win.EventSlug, win.ConditionID)
		// v11.9: stale_missing_peers when no peer prices materialised.
		if peerN == 0 {
			if err := w.sink.CloseWindow(ctx, win.ID, observed, 0, 0, "stale_missing_peers"); err != nil {
				w.log.Warn("repricing: close no-peers failed", "id", win.ID, "err", err)
				continue
			}
			n++
			continue
		}
		peerMove := peer - win.BaselinePrice
		lag := peerMove - observed
		status := "closed_no_lag"
		// 3-cent floor mirrors REPRICING_LAG_MIN_CENTS default.
		if lag >= 0.03 {
			status = "closed_lag_detected"
		}
		if err := w.sink.CloseWindow(ctx, win.ID, observed, peerMove, lag, status); err != nil {
			w.log.Warn("repricing: close failed", "id", win.ID, "err", err)
			continue
		}
		n++
	}
	return n
}

func (w *Worker) observeRun(status string) {
	if w.met == nil || w.met.StrategyWorkerRuns == nil {
		return
	}
	w.met.StrategyWorkerRuns.WithLabelValues("repricing", status).Inc()
}

func (w *Worker) observeItems(opened, closed, errored int) {
	if w.met == nil || w.met.StrategyWorkerItems == nil {
		return
	}
	c := w.met.StrategyWorkerItems
	if opened > 0 {
		c.WithLabelValues("repricing", "persisted").Add(float64(opened + closed))
	}
	if errored > 0 {
		c.WithLabelValues("repricing", "errored").Add(float64(errored))
	}
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyWorkerLatency == nil {
		return
	}
	w.met.StrategyWorkerLatency.WithLabelValues("repricing").Observe(d.Seconds())
}
