// Package strategyvalue is the v11.6 PART 6 shadow value evaluator.
//
// Purpose: backfill `clv_15m`, `clv_1h`, `clv_6h`, `clv_24h`, and a
// "reversal_15m" feature into existing polymarket_strategy_shadow_decisions
// rows whose value columns are still NULL. The worker exists so a
// shadow row that fired but never produced a live alert (and therefore
// never picked up the drift worker's CLV pass) still gets value
// attribution.
//
// Determinism: pure orchestration over three narrow interfaces
// (PendingLister, PriceFetcher, RowUpdater). Tests use in-memory
// fakes; production wraps repositories built on the existing
// polymarket_trades table.
package strategyvalue

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// PendingRow is the shadow row the worker needs to fill.
type PendingRow struct {
	ID            int64
	StrategyName  string
	ConditionID   string
	OutcomeToken  string
	Side          string // BUY / SELL / YES / NO
	BaselinePrice float64
	FiredAt       time.Time
	// Already-resolved windows so the worker can idempotently SKIP
	// values it has already computed.
	CLV15m *float64
	CLV1h  *float64
	CLV6h  *float64
	CLV24h *float64
}

// PendingLister returns up to limit shadow rows whose value columns
// are still NULL and whose fired_at + maxAge has not elapsed.
type PendingLister interface {
	ListPendingValueRows(ctx context.Context, maxAge time.Duration, limit int) ([]PendingRow, error)
}

// PriceFetcher returns the first trade price on (condition_id, outcome_token)
// at or after `at`. ok=false on miss. Production reads from
// polymarket_trades.
type PriceFetcher interface {
	FirstPriceAtOrAfter(ctx context.Context, conditionID, outcomeToken string, at time.Time) (price float64, ok bool, err error)
}

// RowUpdater persists the computed values. Implementation is
// idempotent — the sqlc query only writes columns that are still
// NULL on the row, so a re-run can never overwrite a previously
// computed value.
type RowUpdater interface {
	UpdateValues(ctx context.Context, id int64, vals Values) error
}

// Values bundles the per-row update payload. nil-pointer fields are
// not persisted (NULL stays NULL).
type Values struct {
	CLV15m        *float64
	CLV1h         *float64
	CLV6h         *float64
	CLV24h        *float64
	Reversal15m   *bool
	OutcomeStatus *string
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
	Enabled   bool
	Interval  time.Duration
	BatchSize int
	MaxAge    time.Duration
}

// Window is one (label, duration) pair the worker evaluates.
type Window struct {
	Label string
	Delta time.Duration
}

var defaultWindows = []Window{
	{Label: "15m", Delta: 15 * time.Minute},
	{Label: "1h", Delta: time.Hour},
	{Label: "6h", Delta: 6 * time.Hour},
	{Label: "24h", Delta: 24 * time.Hour},
}

// Worker is the shadow value evaluator.
type Worker struct {
	cfg     Config
	rows    PendingLister
	prices  PriceFetcher
	updater RowUpdater
	met     *metrics.Metrics
	log     Logger
	clock   func() time.Time
	mu      sync.Mutex
	windows []Window
}

func New(cfg Config, rows PendingLister, prices PriceFetcher, updater RowUpdater, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:     cfg,
		rows:    rows,
		prices:  prices,
		updater: updater,
		met:     met,
		log:     log,
		clock:   time.Now,
		windows: defaultWindows,
	}
}

func (w *Worker) WithClock(fn func() time.Time) *Worker {
	if fn != nil {
		w.clock = fn
	}
	return w
}

// WithWindows overrides the per-tick window set. Mostly used by
// tests; production sticks to the spec's 15m/1h/6h/24h.
func (w *Worker) WithWindows(ws []Window) *Worker {
	if len(ws) > 0 {
		w.windows = ws
	}
	return w
}

// Run drives the ticker until ctx cancels. Immediate first tick.
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

// Tick processes one batch.
func (w *Worker) Tick(ctx context.Context) {
	start := w.clock()
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("strategyvalue: panic", nil, "panic", r)
		}
		w.observeLatency(time.Since(start))
	}()

	if w.rows == nil || w.prices == nil || w.updater == nil {
		w.observeStatus("skipped", "")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	rows, err := w.rows.ListPendingValueRows(ctx, w.cfg.MaxAge, w.cfg.BatchSize)
	if err != nil {
		w.observeStatus("failed", "")
		w.log.Error("strategyvalue: list failed", err)
		return
	}
	if len(rows) == 0 {
		w.observeStatus("empty", "")
		return
	}
	now := w.clock()
	for _, r := range rows {
		w.handleOne(ctx, r, now)
	}
}

func (w *Worker) handleOne(ctx context.Context, r PendingRow, now time.Time) {
	vals := Values{}
	any := false
	var first15m *float64
	for _, win := range w.windows {
		if !w.windowEligible(r, win, now) {
			continue
		}
		if alreadyHave(r, win.Label) {
			continue
		}
		// Reference price = first trade at-or-after fired_at + delta.
		ref, ok, err := w.prices.FirstPriceAtOrAfter(ctx, r.ConditionID, r.OutcomeToken, r.FiredAt.Add(win.Delta))
		if err != nil {
			w.observeStatus("price_failed", win.Label)
			w.log.Warn("strategyvalue: price fetch failed", "id", r.ID, "win", win.Label, "err", err)
			continue
		}
		if !ok {
			w.observeMissingPrice(win.Label)
			continue
		}
		move := signedMove(r.Side, r.BaselinePrice, ref)
		any = true
		switch win.Label {
		case "15m":
			vals.CLV15m = floatPtr(move)
			first15m = floatPtr(move)
		case "1h":
			vals.CLV1h = floatPtr(move)
		case "6h":
			vals.CLV6h = floatPtr(move)
		case "24h":
			vals.CLV24h = floatPtr(move)
		}
		w.observeStatus("ok", win.Label)
	}
	// Reversal heuristic: signed_move_15m positive but signed_move_1h
	// crossed back through 0. Only flips Reversal15m when both 15m
	// and 1h were computed this tick.
	if first15m != nil && vals.CLV1h != nil {
		rev := *first15m > 0 && *vals.CLV1h <= 0
		vals.Reversal15m = &rev
	}
	if !any {
		return
	}
	if err := w.updater.UpdateValues(ctx, r.ID, vals); err != nil {
		w.observeStatus("update_failed", "")
		w.log.Warn("strategyvalue: update failed", "id", r.ID, "err", err)
		return
	}
}

func (w *Worker) windowEligible(r PendingRow, win Window, now time.Time) bool {
	// Eligible only when the post-fire window has fully elapsed.
	return now.Sub(r.FiredAt) >= win.Delta
}

func alreadyHave(r PendingRow, label string) bool {
	switch label {
	case "15m":
		return r.CLV15m != nil
	case "1h":
		return r.CLV1h != nil
	case "6h":
		return r.CLV6h != nil
	case "24h":
		return r.CLV24h != nil
	}
	return false
}

// signedMove returns the favourable cents move for the alert
// direction. BUY/YES: ref - baseline. SELL/NO: baseline - ref.
func signedMove(side string, baseline, ref float64) float64 {
	switch side {
	case "SELL", "NO":
		return (baseline - ref) * 100 // → cents
	default:
		return (ref - baseline) * 100
	}
}

func floatPtr(v float64) *float64 { return &v }

func (w *Worker) observeStatus(status, window string) {
	if w.met == nil || w.met.StrategyValueEvalTotal == nil {
		return
	}
	w.met.StrategyValueEvalTotal.WithLabelValues(status, window).Inc()
}

func (w *Worker) observeMissingPrice(window string) {
	if w.met == nil || w.met.StrategyValueEvalMissingPrice == nil {
		return
	}
	w.met.StrategyValueEvalMissingPrice.WithLabelValues(window).Inc()
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.met == nil || w.met.StrategyValueEvalLatency == nil {
		return
	}
	w.met.StrategyValueEvalLatency.Observe(d.Seconds())
}

// ErrNoPendingRows is returned when callers prefer a sentinel.
var ErrNoPendingRows = errors.New("strategyvalue: no pending rows")
