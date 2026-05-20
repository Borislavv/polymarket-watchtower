// Package predictionarchival is the v10.3 cleanup worker. It runs
// every Interval and does two deterministic sweeps:
//
//  1. Terminal-retention: predictions in {resolved, invalidated,
//     stale, already_priced} older than TerminalRetention get
//     archived_at stamped. They stop appearing in the active list
//     but the row is retained for historical analysis.
//
//  2. Stale-no-signal: active predictions whose updated_at is older
//     than StaleNoSignalAfter are flipped to state=stale with a
//     deterministic reason. Operator's safety net for predictions
//     the evolution worker abandoned.
//
// No AI. No Telegram (except an optional aggregated summary which
// is a follow-up). Fail-open: per-row failures log and continue.
package predictionarchival

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/workerguard"
)

// Config tunes the worker.
type Config struct {
	Enabled            bool
	Interval           time.Duration
	TerminalRetention  time.Duration
	StaleNoSignalAfter time.Duration
	BlockedRevalidate  time.Duration
	BatchSize          int
	Clock              func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 1 * time.Hour
	}
	if c.TerminalRetention <= 0 {
		c.TerminalRetention = 72 * time.Hour
	}
	if c.StaleNoSignalAfter <= 0 {
		c.StaleNoSignalAfter = 18 * time.Hour
	}
	if c.BlockedRevalidate <= 0 {
		c.BlockedRevalidate = 6 * time.Hour
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 200
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Store is the seam to PredictionIntelligenceRepository's archival
// API.
type Store interface {
	ListPredictionsForArchival(ctx context.Context, olderThan time.Time, limit int32) ([]repository.ArchivalCandidate, error)
	ArchivePrediction(ctx context.Context, id int64, terminalReason string) error
	ListPredictionsForStaleSignal(ctx context.Context, olderThan time.Time, limit int32) ([]repository.ArchivalCandidate, error)
	MarkPredictionStaleNoSignal(ctx context.Context, id int64, reason string) error
}

// Worker is the periodic cleanup loop.
type Worker struct {
	cfg   Config
	store Store
	met   *metrics.Metrics
	log   *zerolog.Logger
}

func New(cfg Config, store Store, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{cfg: cfg, store: store, met: met, log: log}
}

// Run blocks until ctx cancels. Uses workerguard so a slow Tick
// doesn't overlap with the next Ticker fire.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	guard := workerguard.New("prediction_archival", w.met, w.log)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	guard.Run(ctx, func(ctx context.Context) { w.Tick(ctx) })
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			guard.Run(ctx, func(ctx context.Context) { w.Tick(ctx) })
		}
	}
}

// Summary is the per-cycle outcome the CLI smoke can print.
type Summary struct {
	Archived int
	Staled   int
}

// Tick runs one cycle. Always returns; fail-open on per-row errors.
func (w *Worker) Tick(ctx context.Context) Summary {
	sum := Summary{}
	now := w.cfg.Clock()

	// Sweep 1 — terminal-retention archival.
	archCutoff := now.Add(-w.cfg.TerminalRetention)
	terminal, err := w.store.ListPredictionsForArchival(ctx, archCutoff, int32(w.cfg.BatchSize))
	if err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Msg("prediction archival: list terminal failed")
		}
	}
	for _, c := range terminal {
		reason := "terminal_retention_" + c.CurrentState
		if err := w.store.ArchivePrediction(ctx, c.ID, reason); err != nil {
			if w.log != nil {
				w.log.Warn().Err(err).Int64("id", c.ID).Msg("prediction archival: archive failed")
			}
			continue
		}
		sum.Archived++
		w.observeArchived(c.CurrentState, reason)
	}

	// Sweep 2 — stale-no-signal flip.
	staleCutoff := now.Add(-w.cfg.StaleNoSignalAfter)
	staleCandidates, err := w.store.ListPredictionsForStaleSignal(ctx, staleCutoff, int32(w.cfg.BatchSize))
	if err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Msg("prediction archival: list stale failed")
		}
	}
	for _, c := range staleCandidates {
		reason := "no_signal_for_" + w.cfg.StaleNoSignalAfter.String()
		if err := w.store.MarkPredictionStaleNoSignal(ctx, c.ID, reason); err != nil {
			if w.log != nil {
				w.log.Warn().Err(err).Int64("id", c.ID).Msg("prediction archival: mark stale failed")
			}
			continue
		}
		sum.Staled++
		w.observeStaled("no_signal")
	}

	if w.log != nil && (sum.Archived > 0 || sum.Staled > 0) {
		w.log.Info().
			Int("archived", sum.Archived).
			Int("staled", sum.Staled).
			Msg("prediction archival: cycle complete")
	}
	return sum
}

func (w *Worker) observeArchived(state, reason string) {
	if w.met == nil || w.met.PredictionArchived == nil {
		return
	}
	w.met.PredictionArchived.WithLabelValues(state, reason).Inc()
}

func (w *Worker) observeStaled(reason string) {
	if w.met == nil || w.met.PredictionStaled == nil {
		return
	}
	w.met.PredictionStaled.WithLabelValues(reason).Inc()
}
