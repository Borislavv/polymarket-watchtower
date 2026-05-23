// Package strategypromotion is the v11.6 PART 7 promotion eligibility
// evaluator. It encodes the promotion criteria from CLAUDE.md as
// code: N firings, median signed_move_6h uplift, reversal_15m ratio,
// alerts/day cap. The worker is read-only against
// polymarket_strategy_shadow_decisions; it never modifies a shadow
// row. The decision lands in polymarket_strategy_promotion_reviews
// for the bus to consult.
//
// The bus uses Gate.Allow(strategy) to decide whether a non-shadow
// write is allowed. Promotion takes effect ONLY when ALL of:
//   - STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true
//   - the strategy's *_SHADOW_ONLY=false
//   - the latest review row has eligible=true
//   - the operator has not set STRATEGY_PROMOTION_BYPASS_EXPLICIT=true
//     (which forces shadow regardless of the above)
//
// Operator-flag-alone is intentionally insufficient — that's the
// ТЗ requirement: an operator flipping ShadowOnly=false without a
// passing review must NOT produce live writes.
package strategypromotion

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Sample is the per-strategy aggregate over a lookback window.
type Sample struct {
	StrategyName       string
	StrategyVersion    string
	SampleSize         int
	MedianSignedMove6h float64 // in cents
	Reversal15mRatio   float64 // 0..1
	AlertsPerDay       float64
}

// SampleLister returns the aggregates for each strategy version.
type SampleLister interface {
	ListPromotionSamples(ctx context.Context, lookback time.Duration) ([]Sample, error)
}

// ReviewWriter persists one review row.
type ReviewWriter interface {
	WritePromotionReview(ctx context.Context, r Review) error
}

// Review is the persisted row shape.
type Review struct {
	StrategyName       string
	StrategyVersion    string
	SampleSize         int
	MedianSignedMove6h float64
	Reversal15mRatio   float64
	AlertsPerDay       float64
	Eligible           bool
	Reasons            []string
	ReviewedAt         time.Time
}

// Logger interface.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, err error, kv ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, error, ...any) {}

// Config tunes the criteria.
type Config struct {
	Enabled              bool
	Interval             time.Duration
	Lookback             time.Duration
	MinSampleSize        int
	MinSignedMove6hCents float64
	MaxReversal15mRatio  float64
	MaxAlertsPerDay      float64
	BypassExplicit       bool
}

// Worker drives the periodic review cycle.
type Worker struct {
	cfg     Config
	samples SampleLister
	writer  ReviewWriter
	met     *metrics.Metrics
	log     Logger
	clock   func() time.Time
	mu      sync.Mutex

	// Cached latest eligibility per strategy. Gate.Allow consults
	// this map; the worker rewrites it on each tick.
	stateMu sync.RWMutex
	state   map[string]bool
}

func New(cfg Config, samples SampleLister, writer ReviewWriter, met *metrics.Metrics, log Logger) *Worker {
	if log == nil {
		log = noopLogger{}
	}
	return &Worker{
		cfg:     cfg,
		samples: samples,
		writer:  writer,
		met:     met,
		log:     log,
		clock:   time.Now,
		state:   map[string]bool{},
	}
}

func (w *Worker) WithClock(fn func() time.Time) *Worker {
	if fn != nil {
		w.clock = fn
	}
	return w
}

// Allow is the gate the bus consults before promoting a write to
// non-shadow. Always returns false when BypassExplicit=true OR the
// worker hasn't seen at least one eligible review.
func (w *Worker) Allow(strategy string) bool {
	if w.cfg.BypassExplicit {
		return false
	}
	w.stateMu.RLock()
	defer w.stateMu.RUnlock()
	return w.state[strategy]
}

// Run drives the ticker.
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

// Tick evaluates one batch and writes review rows.
func (w *Worker) Tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("strategypromotion: panic", nil, "panic", r)
		}
	}()
	if w.samples == nil || w.writer == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	rows, err := w.samples.ListPromotionSamples(ctx, w.cfg.Lookback)
	if err != nil {
		w.log.Error("strategypromotion: list samples failed", err)
		return
	}
	now := w.clock()
	newState := map[string]bool{}
	for _, s := range rows {
		r := w.evaluate(s, now)
		if err := w.writer.WritePromotionReview(ctx, r); err != nil {
			w.log.Warn("strategypromotion: write failed", "strategy", s.StrategyName, "err", err)
			continue
		}
		newState[s.StrategyName] = r.Eligible
		w.observeReview(s.StrategyName, r.Eligible)
	}
	w.stateMu.Lock()
	w.state = newState
	w.stateMu.Unlock()
}

// evaluate is the pure deterministic decision logic — exposed so
// tests can drive without the worker plumbing.
func (w *Worker) evaluate(s Sample, now time.Time) Review {
	r := Review{
		StrategyName:       s.StrategyName,
		StrategyVersion:    s.StrategyVersion,
		SampleSize:         s.SampleSize,
		MedianSignedMove6h: s.MedianSignedMove6h,
		Reversal15mRatio:   s.Reversal15mRatio,
		AlertsPerDay:       s.AlertsPerDay,
		ReviewedAt:         now,
	}
	if s.SampleSize < w.cfg.MinSampleSize {
		r.Reasons = append(r.Reasons, "insufficient_sample_size")
	}
	if s.MedianSignedMove6h < w.cfg.MinSignedMove6hCents {
		r.Reasons = append(r.Reasons, "median_uplift_below_floor")
	}
	if w.cfg.MaxReversal15mRatio > 0 && s.Reversal15mRatio > w.cfg.MaxReversal15mRatio {
		r.Reasons = append(r.Reasons, "reversal_ratio_above_ceiling")
	}
	if w.cfg.MaxAlertsPerDay > 0 && s.AlertsPerDay > w.cfg.MaxAlertsPerDay {
		r.Reasons = append(r.Reasons, "alerts_per_day_above_ceiling")
	}
	r.Eligible = len(r.Reasons) == 0
	if r.Eligible {
		r.Reasons = append(r.Reasons, "all_criteria_passed")
	}
	return r
}

func (w *Worker) observeReview(strategy string, eligible bool) {
	if w.met == nil || w.met.StrategyPromotionReviews == nil {
		return
	}
	label := "no"
	if eligible {
		label = "yes"
	}
	w.met.StrategyPromotionReviews.WithLabelValues(strategy, label).Inc()
	if w.met.StrategyPromotionEligible != nil {
		v := 0.0
		if eligible {
			v = 1
		}
		w.met.StrategyPromotionEligible.WithLabelValues(strategy).Set(v)
	}
}
