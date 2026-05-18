// Package sanity owns the soft-delete reaper for polymarket_markets.
//
// Lifecycle (one market):
//
//	active=true ─[disappears from sweep]→ active=false, deleted_at=NOW()
//	                                          │
//	                                          │  (retention elapses, e.g. 30d)
//	                                          ▼
//	[sanity worker re-checks current discover state]
//	                ├──[resumed upstream]──► UpsertMarket clears deleted_at,
//	                │                         flips active=true; sanity also
//	                │                         resets backfill_status='pending'
//	                │                         so missing history is fetched
//	                │
//	                └──[still ended]───────► purged_at=NOW()
//	                                         (market row retained — trades
//	                                          remain queryable; row is
//	                                          excluded from collect/backfill)
//
// Hard delete is intentionally NEVER performed by this worker. The FK
// polymarket_trades.market_id → polymarket_markets(id) has no CASCADE on
// the trade side, so deleting a market row would either fail (FK error)
// or break historical analytics. `purged_at` is the analytics-safe end
// state.
package sanity

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// MarketReaper is the subset of *repository.MarketRepository the worker
// uses. Abstracted as an interface so tests can drive it with fakes.
type MarketReaper interface {
	ListSoftDeletedForPurge(ctx context.Context, cutoff time.Time, claimLimit int32) ([]repository.Market, error)
	MarkPurged(ctx context.Context, marketID int64) error
	RequeueResumed(ctx context.Context, marketID int64) error
}

// UpstreamChecker re-checks whether a soft-deleted market is back in the
// upstream sweep. In production this is a thin wrapper around the
// discover cache (active universe is exactly "markets present in the
// most recent sweep"). Tests inject a fake.
type UpstreamChecker interface {
	IsActiveUpstream(conditionID string) bool
}

// Config tunes the worker.
type Config struct {
	// Interval is the tick cadence. Default 1h.
	Interval time.Duration
	// Retention is the minimum age a soft-deleted market must reach
	// before it is eligible for purge. Default 720h (30d).
	Retention time.Duration
	// ClaimLimit caps the per-tick batch size. Bounds DB load. Default 256.
	ClaimLimit int32
	// Clock overrides time.Now (tests).
	Clock func() time.Time
}

func (c Config) applyDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.Retention <= 0 {
		c.Retention = 720 * time.Hour
	}
	if c.ClaimLimit <= 0 {
		c.ClaimLimit = 256
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// Worker is the long-running soft-delete reaper. Safe to run alongside
// other workers on the same DB; the queries are idempotent.
type Worker struct {
	cfg      Config
	markets  MarketReaper
	upstream UpstreamChecker
	log      *zerolog.Logger
	metrics  *metrics.Metrics
}

// New wires the worker. Both reaper and upstream checker are required.
// metrics may be nil for tests; production wiring passes the shared
// handle so MarketsPurged / MarketsResumed surface on Grafana.
func New(cfg Config, markets MarketReaper, upstream UpstreamChecker, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	return &Worker{cfg: cfg.applyDefaults(), markets: markets, upstream: upstream, log: log, metrics: met}
}

// Run blocks until ctx is cancelled. The initial tick fires immediately so
// a freshly-started process clears any backlog without waiting one
// Interval; subsequent ticks run on the configured cadence.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick runs one reaper sweep; exposed for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	cutoff := w.cfg.Clock().Add(-w.cfg.Retention)
	candidates, err := w.markets.ListSoftDeletedForPurge(ctx, cutoff, w.cfg.ClaimLimit)
	if err != nil {
		w.log.Err(err).Msg("sanity: list candidates failed")
		return
	}
	if len(candidates) == 0 {
		return
	}
	var resumed, purged int
	for _, m := range candidates {
		if ctx.Err() != nil {
			return
		}
		if w.upstream.IsActiveUpstream(m.ConditionID) {
			if err := w.markets.RequeueResumed(ctx, m.ID); err != nil {
				w.log.Err(err).
					Int64("market_id", m.ID).
					Str("condition_id", m.ConditionID).
					Msg("sanity: requeue resumed market failed")
				continue
			}
			resumed++
			if w.metrics != nil {
				w.metrics.MarketsResumed.Inc()
			}
			w.log.Info().
				Int64("market_id", m.ID).
				Str("condition_id", m.ConditionID).
				Msg("sanity: market resumed; deleted_at cleared and backfill re-queued")
			continue
		}
		if err := w.markets.MarkPurged(ctx, m.ID); err != nil {
			w.log.Err(err).
				Int64("market_id", m.ID).
				Str("condition_id", m.ConditionID).
				Msg("sanity: mark purged failed")
			continue
		}
		purged++
		if w.metrics != nil {
			w.metrics.MarketsPurged.Inc()
		}
		w.log.Info().
			Int64("market_id", m.ID).
			Str("condition_id", m.ConditionID).
			Time("soft_deleted_at", m.DeletedAt).
			Msg("sanity: market purged (still ended after retention; trades retained)")
	}
	if resumed > 0 || purged > 0 {
		w.log.Info().
			Int("resumed", resumed).
			Int("purged", purged).
			Int("candidates", len(candidates)).
			Msg("sanity: tick complete")
	}
}
