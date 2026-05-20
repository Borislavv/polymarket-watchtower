// Package workerguard provides a tiny atomic in-flight gate that
// prevents a worker's Tick from overlapping itself. The v10.3 audit
// flagged the prediction-creation + evolution + feedback workers as
// 5–30 minute schedulers whose Ticks can take longer than the
// interval under DB pressure; without a guard the next Ticker fire
// kicks off a second concurrent Tick, doubling DB load.
//
// Guard is process-local + lock-free. The cost is one atomic
// CompareAndSwap per Tick.
//
// Usage:
//
//	guard := workerguard.New("prediction_creation", met, logger)
//	for range ticker.C {
//	    ok := guard.Run(ctx, func(ctx context.Context) {
//	        w.Tick(ctx)
//	    })
//	    _ = ok // false = skipped due to overlap
//	}
package workerguard

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Guard is the cheap in-flight gate.
type Guard struct {
	worker  string
	met     *metrics.Metrics
	log     *zerolog.Logger
	running atomic.Bool
}

// New constructs a Guard. nil metrics + nil logger are tolerated
// (fail-open).
func New(workerName string, met *metrics.Metrics, log *zerolog.Logger) *Guard {
	return &Guard{worker: workerName, met: met, log: log}
}

// Run invokes `fn` under the in-flight gate. Returns true when the
// gate was acquired (and `fn` executed); false when the previous
// invocation is still running and this one was skipped. The skip
// path bumps the skipped_overlap metric and emits a single info-
// level log line so the operator can chart cadence problems.
//
// Latency: one atomic CompareAndSwap on the happy path; no allocs.
func (g *Guard) Run(ctx context.Context, fn func(context.Context)) bool {
	if !g.running.CompareAndSwap(false, true) {
		g.observeSkip("overlap")
		if g.log != nil {
			g.log.Info().Str("worker", g.worker).Msg("worker overlap: previous tick still running; this tick skipped")
		}
		return false
	}
	defer g.running.Store(false)
	start := time.Now()
	fn(ctx)
	g.observeDuration(time.Since(start))
	return true
}

// RunWithTimeout wraps `fn` in a timeout-bound context so a stuck
// Tick can't keep the gate held forever. The guard releases when
// `fn` returns (or panics; the deferred Store covers both).
func (g *Guard) RunWithTimeout(parent context.Context, timeout time.Duration, fn func(context.Context)) bool {
	if timeout <= 0 {
		return g.Run(parent, fn)
	}
	if !g.running.CompareAndSwap(false, true) {
		g.observeSkip("overlap")
		if g.log != nil {
			g.log.Info().Str("worker", g.worker).Msg("worker overlap: previous tick still running; this tick skipped")
		}
		return false
	}
	defer g.running.Store(false)
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	fn(ctx)
	g.observeDuration(time.Since(start))
	return true
}

func (g *Guard) observeSkip(reason string) {
	if g.met == nil || g.met.WorkerCycleSkipped == nil {
		return
	}
	g.met.WorkerCycleSkipped.WithLabelValues(g.worker, reason).Inc()
}

func (g *Guard) observeDuration(d time.Duration) {
	if g.met == nil || g.met.WorkerCycleDuration == nil {
		return
	}
	g.met.WorkerCycleDuration.WithLabelValues(g.worker).Observe(d.Seconds())
}
