// Package strategybus is the orchestration façade every v11.5
// detector calls into. It centralises:
//
//   - per-strategy enabled / shadow-only flag enforcement;
//   - the routing decision (write to shadow ledger vs. emit live);
//   - the metrics + log line per decision;
//   - the promotion gate.
//
// The bus has ONE write surface (Record) and one decision surface
// (ShouldEvaluate). Detectors / workers call ShouldEvaluate before
// the (typically free) Decide() so we don't waste CPU on a disabled
// path; Record persists the shadow row.
//
// The bus does NOT call detect.Loop, AI, OpenAI, or Telegram. It
// also does NOT decide promotion — promotion is operator-driven.
// What the bus does honor is the bookkeeping invariant that, with
// GlobalPromotionAllowed=false, NO live row can ever leak out (the
// bus rewrites Decision.ShadowOnly=true unconditionally).
package strategybus

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// StrategyFlag carries the per-strategy on/off + shadow-only knobs.
// Mirrors the env-loaded config block so detect.Loop / workers can
// avoid importing the entire app config.
type StrategyFlag struct {
	Name       string
	Enabled    bool
	ShadowOnly bool
}

// PromotionGate is the v11.6 hook the bus consults BEFORE letting a
// non-shadow row through. The strategypromotion.Worker satisfies it.
// nil = no gate (legacy v11.5 behaviour where ShadowOnly per-flag +
// GlobalPromotionAllowed is the only test).
type PromotionGate interface {
	Allow(strategy string) bool
}

// Config wires the bus.
type Config struct {
	StrategyVersion        string
	GlobalPromotionAllowed bool

	// Flags is the closed set the bus recognises. Calls for any
	// unknown strategy name are dropped (with a metric label
	// "unknown_strategy") rather than fanning out to the writer.
	Flags map[string]StrategyFlag

	// PromotionGate, when set, must return true for a strategy
	// before the bus emits a non-shadow row. Forces ShadowOnly=true
	// for the row when the gate denies. nil gate = legacy v11.5
	// behaviour (no extra check).
	PromotionGate PromotionGate
}

// Logger keeps the package dependency-free.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, err error, kv ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, error, ...any) {}

// Bus is the orchestration façade.
type Bus struct {
	cfg    Config
	writer shadowdecisions.Writer
	met    *metrics.Metrics
	log    Logger
	clock  func() time.Time
	mu     sync.RWMutex
}

// New constructs a Bus. A nil writer is treated as NopWriter so
// the bus is always safe to call.
func New(cfg Config, writer shadowdecisions.Writer, met *metrics.Metrics, log Logger) *Bus {
	if writer == nil {
		writer = shadowdecisions.NopWriter{}
	}
	if log == nil {
		log = noopLogger{}
	}
	if cfg.Flags == nil {
		cfg.Flags = map[string]StrategyFlag{}
	}
	return &Bus{
		cfg:    cfg,
		writer: writer,
		met:    met,
		log:    log,
		clock:  time.Now,
	}
}

// WithClock overrides the wall clock; used by tests.
func (b *Bus) WithClock(fn func() time.Time) *Bus {
	if fn != nil {
		b.clock = fn
	}
	return b
}

// ShouldEvaluate returns true when the named strategy is enabled
// in config. Detectors should skip Decide() when this returns
// false to avoid paying for graph / wallet-line reads.
func (b *Bus) ShouldEvaluate(strategyName string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	flag, ok := b.cfg.Flags[strategyName]
	if !ok {
		return false
	}
	return flag.Enabled
}

// IsShadowOnly returns the effective shadow-only flag for a
// strategy. Shadow-only is true when the per-strategy flag is true
// OR when GlobalPromotionAllowed is false.
func (b *Bus) IsShadowOnly(strategyName string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	flag, ok := b.cfg.Flags[strategyName]
	if !ok {
		return true
	}
	if !b.cfg.GlobalPromotionAllowed {
		return true
	}
	return flag.ShadowOnly
}

// Record persists one decision. The bus rewrites ShadowOnly to
// true when promotion is not allowed for the strategy; live writes
// only happen when GlobalPromotionAllowed=true AND the per-strategy
// ShadowOnly=false.
//
// FiredAt defaults to clock() when zero.
// StrategyVersion is auto-stamped if empty.
//
// Returns the inserted row id (0 with the NopWriter or on error).
func (b *Bus) Record(ctx context.Context, d shadowdecisions.Decision) (int64, error) {
	flag, ok := b.cfg.Flags[d.StrategyName]
	if !ok {
		b.observeUnknown(d.StrategyName)
		return 0, nil
	}
	if !flag.Enabled {
		// Disabled strategies do not write rows. (A future operator
		// dry-run mode could flip this but it would require an explicit
		// "audit" flag, not present today.)
		return 0, nil
	}

	if d.StrategyVersion == "" {
		d.StrategyVersion = b.cfg.StrategyVersion
	}
	if d.FiredAt.IsZero() {
		d.FiredAt = b.clock()
	}
	// Promotion gate: a strategy can only emit a non-shadow row when
	// ALL of:
	//   - GlobalPromotionAllowed=true,
	//   - per-strategy ShadowOnly=false,
	//   - PromotionGate.Allow(strategy)=true (v11.6 — operator flag
	//     alone is insufficient).
	// Any failure forces shadow_only=true.
	if !b.cfg.GlobalPromotionAllowed || flag.ShadowOnly {
		d.ShadowOnly = true
	}
	if !d.ShadowOnly && b.cfg.PromotionGate != nil && !b.cfg.PromotionGate.Allow(d.StrategyName) {
		d.ShadowOnly = true
	}

	id, err := b.writer.Record(ctx, d)
	if err != nil {
		b.observeWriteError(d.StrategyName)
		b.log.Warn("strategybus: shadow write failed", "strategy", d.StrategyName, "err", err)
		return 0, err
	}
	b.observeDecision(d)
	return id, nil
}

// observeDecision emits the v11.5 metrics. Score / confidence go
// through histograms; the count goes through the labelled counter.
func (b *Bus) observeDecision(d shadowdecisions.Decision) {
	if b.met == nil {
		return
	}
	if b.met.StrategyShadowDecisionsTotal != nil {
		b.met.StrategyShadowDecisionsTotal.WithLabelValues(
			d.StrategyName, string(d.Kind), string(d.Level)).Inc()
	}
	if b.met.StrategyShadowScoreBucket != nil {
		b.met.StrategyShadowScoreBucket.WithLabelValues(d.StrategyName).Observe(d.Score)
	}
	if b.met.StrategyShadowConfidenceBucket != nil {
		b.met.StrategyShadowConfidenceBucket.WithLabelValues(d.StrategyName).Observe(d.Confidence)
	}
}

func (b *Bus) observeUnknown(name string) {
	if b.met == nil || b.met.StrategyShadowDecisionsTotal == nil {
		return
	}
	b.met.StrategyShadowDecisionsTotal.WithLabelValues(name, "unknown_strategy", "none").Inc()
}

func (b *Bus) observeWriteError(name string) {
	if b.met == nil || b.met.StrategyShadowWriteErrors == nil {
		return
	}
	b.met.StrategyShadowWriteErrors.WithLabelValues(name).Inc()
}

// StrategyNames is the canonical list of v11.5 strategy names.
// Callers wiring the Flags map should iterate this slice so a
// future detector can be added by editing one place.
var StrategyNames = []string{
	"thesisaccum",
	"holderdelta",
	"catalystwindow",
	"bookvacuum",
	"repricinglag",
	"walletcohort",
	"conflictresolve",
	"rulesrisk",
	"cheaptail",
}
