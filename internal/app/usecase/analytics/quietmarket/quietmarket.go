// Package quietmarket evaluates whether a market/outcome was historically
// quiet and is now seeing a meaningful event. It is a CONTEXT detector —
// it does NOT fire alerts on its own. The single-trade scorer or the
// accumulation detector decides whether to fire; this package is invoked
// afterwards and stamps a structured "quiet-market wake-up" reason on the
// Finding so operators see why the alert is interesting in context.
//
// The signal we are surfacing:
//
//	A market/outcome was historically quiet,
//	then suddenly receives a large directional trade or accumulation line,
//	especially late in the lifecycle.
//
// All inputs come from already-persisted history (dbbaseline.Provider /
// repository.TradeRepository). There is no parallel scoring engine, no
// shadow threshold ladder — the detector is a pure predicate over numbers
// the rest of the pipeline already has.
package quietmarket

import (
	"time"
)

// Config tunes the detector.
//
//   - Enabled is the master switch. When false, Decide always returns an
//     empty (non-qualifying) Verdict — no work runs.
//   - MaxTradesPerDay is the rate ceiling: a market that averages MORE
//     than this many trades per day in its baseline window is NOT quiet.
//   - MaxNotionalPerDayUSD is the dollar-volume ceiling, same direction.
//     A market can have many tiny trades and still be "quiet" by dollars,
//     or vice versa — both ceilings must hold for the quiet verdict.
//   - MinIdleDuration is the floor on the gap between the wake-up event
//     and the previous historical trade. Catches the canonical pattern
//     "market saw nothing for half a day, then a $25k bet arrived."
//   - MinCurrentNotionalUSD filters tiny events. A $50 bet on a quiet
//     market is not a wake-up signal.
//   - MinMultiplier is the rarity floor: current event notional /
//     marketMedian. Optional (0 disables). Catches the case where the
//     market has been quiet but the event is still close to the median —
//     not really a "wake-up", just normal trickle activity.
type Config struct {
	Enabled               bool
	MaxTradesPerDay       float64
	MaxNotionalPerDayUSD  float64
	MinIdleDuration       time.Duration
	MinCurrentNotionalUSD float64
	MinMultiplier         float64
}

// History is the snapshot of past activity the detector reads. Populated
// upstream from BaselineDistribution + the new LastTradeAtBefore query.
type History struct {
	// SampleCount and TotalNotionalUSD are the baseline window totals
	// (matching dbbaseline.Provider). Span is the observed
	// newest−oldest distance, NOT the configured window cap.
	SampleCount      int64
	TotalNotionalUSD float64
	Span             time.Duration
	// MarketMedianUSD is the per-bucket median. Used for the optional
	// MinMultiplier gate. 0 means "no median available".
	MarketMedianUSD float64
	// LastTradedAt is the timestamp of the most recent historical trade
	// STRICTLY BEFORE the wake-up event. Zero when there is no prior
	// trade (Decide treats that as "infinite idle" — qualifies if other
	// gates pass).
	LastTradedAt time.Time
}

// Event is the current activity being considered for a wake-up tag. For a
// single-trade alert this is the trade itself; for an accumulation line
// alert this is the line as a whole (NotionalUSD = lineTotal, At =
// line.NewestAt or line.OldestAt — caller's choice; the gate is symmetric).
type Event struct {
	NotionalUSD float64
	At          time.Time
}

// Verdict is the structured outcome.
type Verdict struct {
	// Qualifies is true when ALL configured gates passed. False otherwise.
	Qualifies bool
	// Reason is "QUIET_MARKET_WAKEUP" when Qualifies, empty otherwise.
	// Surfaced into Finding.Reasons so operators see the tag verbatim.
	Reason string

	// Diagnostic fields — populated whenever the detector ran (even when
	// gates didn't clear). The Telegram formatter prints them so a near-
	// miss is auditable without re-querying the DB.
	TradesPerDay      float64
	NotionalPerDayUSD float64
	IdleDuration      time.Duration
	BaselineSpan      time.Duration
}

// ReasonQuietMarketWakeup is the canonical reason code surfaced in
// Finding.Reasons + structured logs + tests.
const ReasonQuietMarketWakeup = "QUIET_MARKET_WAKEUP"

// Detector is a pure value. Safe for concurrent use; no internal state.
type Detector struct {
	cfg Config
}

// New constructs a Detector. The config is used as-is — defaults are the
// operator's responsibility (we deliberately do NOT silently fill zero
// values so a misconfigured prod env fails closed, not on heuristics).
func New(cfg Config) *Detector { return &Detector{cfg: cfg} }

// Config returns the applied configuration.
func (d *Detector) Config() Config { return d.cfg }

// Decide runs the gates against (history, event) and reports the verdict.
//
// Gate order (all must clear for Qualifies=true):
//
//  1. cfg.Enabled
//  2. history readiness: SampleCount ≥ 1, Span > 0 (without these we
//     cannot compute per-day rates and so cannot judge "quiet")
//  3. event size: event.NotionalUSD ≥ cfg.MinCurrentNotionalUSD
//  4. activity ceilings: tradesPerDay ≤ MaxTradesPerDay AND
//     notionalPerDay ≤ MaxNotionalPerDayUSD
//  5. idle floor: when LastTradedAt is non-zero, gap ≥ MinIdleDuration
//     (zero LastTradedAt means "never traded before", which is the
//     strongest possible quiet signal — passes by default)
//  6. multiplier floor: when MinMultiplier > 0 AND MarketMedianUSD > 0,
//     event.NotionalUSD / MarketMedianUSD ≥ MinMultiplier
func (d *Detector) Decide(hist History, event Event) Verdict {
	v := Verdict{BaselineSpan: hist.Span}
	if !d.cfg.Enabled {
		return v
	}
	if hist.SampleCount < 1 || hist.Span <= 0 {
		return v
	}
	days := hist.Span.Hours() / 24
	if days <= 0 {
		return v
	}
	v.TradesPerDay = float64(hist.SampleCount) / days
	v.NotionalPerDayUSD = hist.TotalNotionalUSD / days
	if !hist.LastTradedAt.IsZero() {
		v.IdleDuration = event.At.Sub(hist.LastTradedAt)
		if v.IdleDuration < 0 {
			v.IdleDuration = 0
		}
	}

	if event.NotionalUSD < d.cfg.MinCurrentNotionalUSD {
		return v
	}
	if v.TradesPerDay > d.cfg.MaxTradesPerDay {
		return v
	}
	if v.NotionalPerDayUSD > d.cfg.MaxNotionalPerDayUSD {
		return v
	}
	if d.cfg.MinIdleDuration > 0 && !hist.LastTradedAt.IsZero() && v.IdleDuration < d.cfg.MinIdleDuration {
		return v
	}
	if d.cfg.MinMultiplier > 0 && hist.MarketMedianUSD > 0 {
		if event.NotionalUSD/hist.MarketMedianUSD < d.cfg.MinMultiplier {
			return v
		}
	}

	v.Qualifies = true
	v.Reason = ReasonQuietMarketWakeup
	return v
}
