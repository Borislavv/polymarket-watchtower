// Package strategyoutcome backfills outcome_status onto shadow rows
// that link to a fully-resolved alert. The worker is read-only
// against polymarket_alerts and idempotent: it only writes to
// shadow rows whose outcome_status is still NULL.
//
// This intentionally uses the existing v11.4 outcome-resolution
// pipeline (`polymarket_alerts.outcome_status`) as the source of
// truth — no external network. Standalone shadow rows that never
// linked to an alert remain NULL until a future evaluator picks
// them up directly from market resolutions.
package strategyoutcome

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// PendingRow is one shadow row + the resolved alert outcome.
type PendingRow struct {
	ID           int64
	AlertOutcome string // "resolved_correct" | "resolved_wrong" | "unknown" | "unavailable"
	DedupKey     string
}

// Lister returns rows ready for backfill.
type Lister interface {
	ListShadowRowsForOutcomeBackfill(ctx context.Context, limit int) ([]PendingRow, error)
}

// Updater persists the outcome_status on one row.
type Updater interface {
	UpdateShadowOutcomeStatus(ctx context.Context, id int64, outcome string) error
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
	Enabled   bool
	Interval  time.Duration
	BatchSize int
}

// Worker drives the periodic backfill cycle.
type Worker struct {
	cfg     Config
	lister  Lister
	updater Updater
	met     *metrics.Metrics
	log     Logger
	mu      sync.Mutex
	clock   func() time.Time
}

func New(cfg Config, lister Lister, updater Updater, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:     cfg,
		lister:  lister,
		updater: updater,
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
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("strategyoutcome: panic", nil, "panic", r)
		}
	}()
	if w.lister == nil || w.updater == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	rows, err := w.lister.ListShadowRowsForOutcomeBackfill(ctx, w.cfg.BatchSize)
	if err != nil {
		w.log.Error("strategyoutcome: list failed", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	for _, r := range rows {
		if r.AlertOutcome == "" {
			continue
		}
		if err := w.updater.UpdateShadowOutcomeStatus(ctx, r.ID, r.AlertOutcome); err != nil {
			w.log.Warn("strategyoutcome: update failed", "id", r.ID, "err", err)
			continue
		}
	}
}
