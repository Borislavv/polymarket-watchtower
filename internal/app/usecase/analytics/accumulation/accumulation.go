// Package accumulation detects the same-trader same-(market,outcome,side)
// accumulation-line signal: one wallet repeatedly building exposure to a
// single side of a single outcome. This is a qualitatively distinct
// surveillance signal from:
//
//   - single whale (one huge bet that fires by absolute + multiplier),
//   - multi-wallet cluster (several wallets converging on a category).
//
// The accumulation line catches the "many smalls" shape that the
// single-trade scorer cannot see: 200 trades × $200 = $40k is invisible
// trade-by-trade but is a strong informed-flow candidate as a whole.
//
// Severity is anchored on the existing Info/Warning/Critical thresholds —
// there is no parallel threshold universe. Two size paths qualify a line
// at tier T:
//
//   - (meaningful)  medianTrade ≥ FRACTION × T.MinNotionalUSD
//     AND lineTotal ≥ TotalMultiplier × T.MinNotionalUSD
//   - (many-smalls) lineTotal ≥ ManySmallsMultiplier × T.MinNotionalUSD
//     (catches 200 × $200 = $40k vs Info $5k)
//
// On top of the size path, every tier also requires:
//
//   - trades ≥ tierMinTrades(T)   (Info=3, Warning=4, Critical=5)
//   - avg_odds ≥ T.MinOdds         (line direction must be asymmetric)
//   - lineTotal / marketMedian ≥ T.MinMultiplier  (line is rare vs market)
//
// Hard is reserved for: trades ≥ 5 AND lineTotal ≥ HardMultiplier ×
// CRITICAL.MinNotionalUSD AND HOT lifecycle (or other strong context).
//
// The package is pure: no I/O. The repository read happens upstream and
// is passed in as a Line.
package accumulation

import (
	"math"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
)

// ReasonCode is a structured tag describing WHY a line qualified. Multiple
// codes can apply at once and surface in the alert payload so an operator
// can see the shape at a glance without re-deriving it from raw numbers.
type ReasonCode string

const (
	// Surveillance shape codes — v6 vocabulary.
	ReasonSameTraderAccumulation ReasonCode = "SAME_TRADER_ACCUMULATION"
	ReasonLinePayoffHigh         ReasonCode = "LINE_PAYOFF_HIGH"
	ReasonLineMarketTail         ReasonCode = "LINE_MARKET_TAIL"
	ReasonLineTraderTail         ReasonCode = "LINE_TRADER_TAIL"
	ReasonManySmallsSameSide     ReasonCode = "MANY_SMALLS_SAME_SIDE"
	ReasonLowBaselineConfidence  ReasonCode = "LOW_BASELINE_CONFIDENCE"
	ReasonLateMarketAccumulation ReasonCode = "LATE_MARKET_ACCUMULATION"
	ReasonHotMarketAccumulation  ReasonCode = "HOT_MARKET_ACCUMULATION"
	ReasonLowSampleSize          ReasonCode = "LOW_SAMPLE_SIZE"

	// Aliases retained for places that still reference the v4 names.
	// They map onto the v6 vocabulary verbatim.
	ReasonRepeatedSameOutcome ReasonCode = "SAME_TRADER_ACCUMULATION"
	ReasonLineTotalHigh       ReasonCode = "LINE_PAYOFF_HIGH"
	ReasonManySmallSameSide   ReasonCode = "MANY_SMALLS_SAME_SIDE"
	ReasonLineLargeVsMarket   ReasonCode = "LINE_MARKET_TAIL"
	ReasonLineLargeVsSelf     ReasonCode = "LINE_TRADER_TAIL"
)

// POSSIBLE_MARKET_MAKER lives in package mmfilter (the semantic owner —
// it's emitted on MM-suppression telemetry, not on accumulation alerts).
// See internal/app/usecase/analytics/mmfilter.ReasonPossibleMarketMaker.

// WindowKind names the time horizon the line was aggregated over. The
// detector's math is identical for both — the kind exists only so the
// caller can carry it through to the dedup namespace and the alert
// payload (an operator should be able to tell at a glance whether a
// firing is a 24h burst or a 90-day slow drip).
type WindowKind string

const (
	// WindowKindRecent is the bursty short-window line (default 24h).
	// Detects fast accumulation; emissions are bucketed by Cooldown so a
	// continuing burst dedupes within the same bucket.
	WindowKindRecent WindowKind = "recent"
	// WindowKindLifetime is the full-history line (since=NULL on the SQL
	// side). Detects slow-drip conviction across days/weeks/months.
	// Emissions dedupe per (line, severity tier) so a line that crosses
	// Info → Warning → Critical emits exactly once at each tier.
	WindowKindLifetime WindowKind = "lifetime"
)

// Line is the input the scorer needs. Repositories project their server-
// side roll-up into this shape so the math runs once over a single value
// type and stays trivially testable.
type Line struct {
	Wallet       string
	MarketID     string
	OutcomeToken string
	Side         trade.Side
	// Window names the horizon this line covers. Purely a tag — does not
	// affect the gating math. The detector passes it through to Verdict
	// so the caller can build a window-aware dedup key without re-
	// deriving the kind from the Since cutoff.
	Window WindowKind

	TradeCount        int
	TotalNotionalUSD  float64
	MeanNotionalUSD   float64
	MedianNotionalUSD float64
	MaxNotionalUSD    float64
	MinNotionalUSD    float64
	AvgOdds           float64
	MaxOdds           float64
	OldestAt          time.Time
	NewestAt          time.Time

	// Context from the surrounding pipeline.
	MarketMedianUSD float64 // 0 when market baseline unready
	MarketP95USD    float64
	TraderMedianUSD float64 // 0 when trader baseline unready
	TraderP95USD    float64
	LifecyclePct    float64 // 0..100, 0 when unknown
	Hot             bool    // matches Finding.Hot semantics
}

// Span returns NewestAt − OldestAt. Zero when fewer than two trades.
func (l Line) Span() time.Duration {
	if l.TradeCount < 2 {
		return 0
	}
	return l.NewestAt.Sub(l.OldestAt)
}

// MarketMultiplier is lineTotal / marketMedian, the line-level analogue of
// the per-trade market multiplier. Returns 0 when market baseline unready.
func (l Line) MarketMultiplier() float64 {
	if l.MarketMedianUSD <= 0 {
		return 0
	}
	return l.TotalNotionalUSD / l.MarketMedianUSD
}

// TraderMultiplier is lineTotal / traderMedian. 0 when trader baseline
// unready. Note: this compares the *cumulative* line against the wallet's
// typical *single* trade — a 200× value means "the wallet has built up
// 200 typical-trades' worth of exposure", which is the surveillance read.
func (l Line) TraderMultiplier() float64 {
	if l.TraderMedianUSD <= 0 {
		return 0
	}
	return l.TotalNotionalUSD / l.TraderMedianUSD
}

// Config tunes the detector. Defaults applied by New.
type Config struct {
	// Enabled is the master switch. When false, Decide always returns a
	// fired=false verdict (no scoring math runs).
	Enabled bool

	// Window is the lookback applied at the repository layer. Surfaced on
	// the Config so the docs+config form one description; the detector
	// itself does not slice the line, it consumes the already-windowed
	// repository result.
	Window time.Duration

	// MinTrades is the floor below which no line ever fires regardless of
	// notional. Default 3.
	MinTrades int

	// TradeFractionOfInfo gates the "meaningful per-trade" size path:
	// medianTrade ≥ TradeFractionOfInfo × tier.MinNotionalUSD. Default 0.60.
	TradeFractionOfInfo float64

	// TotalMultiplier scales the tier notional floor for the line total
	// (meaningful path). Default 2 — a line must clear at least 2× the
	// single-trade tier notional to be considered "accumulation."
	TotalMultiplier float64

	// ManySmallsMultiplier scales the tier notional floor for the
	// "many-smalls" size path: lineTotal ≥ ManySmallsMultiplier × tier
	// notional. Default 4 — a line of small trades alerts when it
	// collectively exceeds 4× the tier's per-trade floor (200×$200 = $40k
	// vs Info $5k: 40000 ≥ 4×5000 ⇒ qualifies).
	ManySmallsMultiplier float64

	// HardMultiplier scales CRITICAL.MinNotionalUSD for the optional Hard
	// promotion. Default 3.
	HardMultiplier float64

	// Cooldown is the inter-alert spacing for the same dedup bucket.
	// Surfaced on Config so the dedup-key derivation can read it. Default
	// 30 minutes.
	Cooldown time.Duration
}

// applyDefaults fills in zero-value fields with sensible production
// defaults. Pure — does not consult the env.
func (c Config) applyDefaults() Config {
	if c.MinTrades <= 0 {
		c.MinTrades = 3
	}
	if c.TradeFractionOfInfo <= 0 {
		c.TradeFractionOfInfo = 0.60
	}
	if c.TotalMultiplier <= 0 {
		c.TotalMultiplier = 2
	}
	if c.ManySmallsMultiplier <= 0 {
		c.ManySmallsMultiplier = 4
	}
	if c.HardMultiplier <= 0 {
		c.HardMultiplier = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 30 * time.Minute
	}
	if c.Window <= 0 {
		c.Window = 24 * time.Hour
	}
	return c
}

// Verdict is the structured outcome of Decide.
type Verdict struct {
	Fired      bool
	Severity   anomaly.Severity
	Score      int     // 0..100 — heuristic surfaced for triage; not a probability
	Confidence float64 // 0..1 — sample-size + readiness adjustment
	Reasons    []ReasonCode
	// Window echoes Line.Window so callers can build a window-aware
	// dedup key off the Verdict alone.
	Window WindowKind
	// Diagnostic fields (always populated, even when Fired=false).
	LineMarketMultiplier float64
	LineTraderMultiplier float64
	SizePath             string // "meaningful" | "many-smalls" | "" (n/a)
}

// Detector evaluates a Line and produces a Verdict. Pure and concurrency-
// safe (no internal state).
type Detector struct {
	cfg        Config
	thresholds anomaly.Thresholds
}

// New constructs a Detector. The supplied Thresholds are the SAME tier
// definitions used by the single-trade scorer — we deliberately do not
// introduce a parallel threshold universe.
func New(cfg Config, t anomaly.Thresholds) *Detector {
	return &Detector{cfg: cfg.applyDefaults(), thresholds: t}
}

// Config returns the applied configuration (with defaults filled).
func (d *Detector) Config() Config { return d.cfg }

// Decide evaluates the line. Empty line, disabled config, or any failed
// gate returns Fired=false with diagnostics populated for the alert
// payload (operators inspecting a near-miss find the numbers there).
func (d *Detector) Decide(line Line) Verdict {
	v := Verdict{
		Window:               line.Window,
		LineMarketMultiplier: line.MarketMultiplier(),
		LineTraderMultiplier: line.TraderMultiplier(),
	}
	if !d.cfg.Enabled {
		return v
	}
	if line.TradeCount < d.cfg.MinTrades {
		return v
	}

	// Walk the ladder top-down and stop at the highest qualifying tier.
	for _, t := range []struct {
		sev  anomaly.Severity
		minN int
		tier anomaly.Tier
	}{
		{anomaly.SeverityCritical, tierMinTrades(anomaly.SeverityCritical), d.thresholds.Critical},
		{anomaly.SeverityWarning, tierMinTrades(anomaly.SeverityWarning), d.thresholds.Warning},
		{anomaly.SeverityInfo, tierMinTrades(anomaly.SeverityInfo), d.thresholds.Info},
	} {
		if line.TradeCount < t.minN {
			continue
		}
		if !d.passesNonSize(line, t.tier) {
			continue
		}
		sizePath := d.passesSize(line, t.tier)
		if sizePath == "" {
			continue
		}
		v.Fired = true
		v.Severity = t.sev
		v.SizePath = sizePath
		v.Reasons = d.reasons(line, t.tier, sizePath)
		v.Confidence = d.confidence(line)
		v.Score = d.score(line, t.tier, t.sev)
		// Hard promotion: very large line + HOT lifecycle.
		if t.sev == anomaly.SeverityCritical && line.Hot &&
			line.TotalNotionalUSD >= d.cfg.HardMultiplier*d.thresholds.Critical.MinNotionalUSD &&
			line.TradeCount >= tierMinTrades(anomaly.SeverityCritical) {
			v.Severity = anomaly.SeverityHard
		}
		return v
	}
	return v
}

// LineProfitIfWinUSD is the line's profit-if-win using the line's
// AVERAGE odds: total × (avgOdds − 1). The line median odds is a
// reasonable approximation for "if this whole position resolves
// favourably, what does the wallet make?".
func (l Line) LineProfitIfWinUSD() float64 {
	if l.AvgOdds <= 1 || l.TotalNotionalUSD <= 0 {
		return 0
	}
	return l.TotalNotionalUSD * (l.AvgOdds - 1)
}

// passesNonSize evaluates the v6 tail+payoff gates for one tier. The
// median-multiplier gate (line_total / market_median ≥ tier.MinMultiplier)
// has been removed — it was the v4/v5 false-positive source for thin
// markets. Replaced with:
//
//   - odds floor (unchanged)
//   - payoff floor: lineProfit ≥ tier.MinProfitUSD
//   - market tail: max(lineTotal/marketP95, lineMax/marketP95) ≥
//     tier.MinMarketP95Ratio (only when market baseline ready)
//   - trader tail: lineTotal/traderP95 ≥ tier.MinTraderP95Ratio
//     (only when trader baseline ready)
//
// Gates with zero floors are disabled (operator turned them off);
// gates whose baseline is unready are unenforced (returning true for
// that specific gate). The detector exposes the readiness state to
// reasons() so the alert payload can carry LowBaselineConfidence.
func (d *Detector) passesNonSize(line Line, t anomaly.Tier) bool {
	if t.MinOdds > 0 && line.AvgOdds < t.MinOdds {
		return false
	}
	if t.MinProfitUSD > 0 && line.LineProfitIfWinUSD() < t.MinProfitUSD {
		return false
	}
	if t.MinMarketP95Ratio > 0 && line.MarketP95USD > 0 {
		// Tail can be tripped by either the cumulative line total or
		// any one of its trades being large. Single-large-trade-in-a-
		// line is a stronger informed signal than slow drip, but slow
		// drip is the *exclusive* shape catched by accumulation, so
		// we accept either.
		lineRatio := line.TotalNotionalUSD / line.MarketP95USD
		maxRatio := line.MaxNotionalUSD / line.MarketP95USD
		if lineRatio < t.MinMarketP95Ratio && maxRatio < t.MinMarketP95Ratio {
			return false
		}
	}
	if t.MinTraderP95Ratio > 0 && line.TraderP95USD > 0 {
		if line.TotalNotionalUSD/line.TraderP95USD < t.MinTraderP95Ratio {
			return false
		}
	}
	return true
}

// passesSize evaluates the two size paths and reports which one qualified
// ("meaningful", "many-smalls", or "" for neither).
func (d *Detector) passesSize(line Line, t anomaly.Tier) string {
	tierNotional := t.MinNotionalUSD
	if tierNotional <= 0 {
		// Misconfiguration — treat as "no floor"; fall back to many-smalls
		// only so we don't accidentally fire on dust.
		if line.TotalNotionalUSD > 0 {
			return "many-smalls"
		}
		return ""
	}
	meaningfulTotalFloor := d.cfg.TotalMultiplier * tierNotional
	manySmallsTotalFloor := d.cfg.ManySmallsMultiplier * tierNotional
	medianFloor := d.cfg.TradeFractionOfInfo * tierNotional

	// Many-smalls dominates "meaningful" on total floor — check it first
	// so a line that satisfies both still names the more discriminating
	// path. (4× is strictly stricter than 2× for the same data.)
	if line.TotalNotionalUSD >= manySmallsTotalFloor {
		return "many-smalls"
	}
	if line.TotalNotionalUSD >= meaningfulTotalFloor && line.MedianNotionalUSD >= medianFloor {
		return "meaningful"
	}
	return ""
}

// reasons compiles the structured tags for the alert payload. Always
// includes SAME_TRADER_ACCUMULATION; the others reflect which gates
// lit up under the v6 tail+payoff model.
func (d *Detector) reasons(line Line, t anomaly.Tier, sizePath string) []ReasonCode {
	out := []ReasonCode{ReasonSameTraderAccumulation}
	if t.MinProfitUSD > 0 && line.LineProfitIfWinUSD() >= 2*t.MinProfitUSD {
		out = append(out, ReasonLinePayoffHigh)
	}
	if sizePath == "many-smalls" {
		out = append(out, ReasonManySmallsSameSide)
	}
	if line.MarketP95USD > 0 {
		ratio := line.TotalNotionalUSD / line.MarketP95USD
		if line.MaxNotionalUSD/line.MarketP95USD > ratio {
			ratio = line.MaxNotionalUSD / line.MarketP95USD
		}
		if t.MinMarketP95Ratio > 0 && ratio >= 2*t.MinMarketP95Ratio {
			out = append(out, ReasonLineMarketTail)
		}
	}
	if line.TraderP95USD > 0 && t.MinTraderP95Ratio > 0 {
		if line.TotalNotionalUSD/line.TraderP95USD >= 2*t.MinTraderP95Ratio {
			out = append(out, ReasonLineTraderTail)
		}
	}
	if line.MarketP95USD <= 0 || line.TraderP95USD <= 0 {
		out = append(out, ReasonLowBaselineConfidence)
	}
	if line.LifecyclePct >= 75 {
		out = append(out, ReasonLateMarketAccumulation)
	}
	if line.Hot {
		out = append(out, ReasonHotMarketAccumulation)
	}
	if line.TradeCount < d.cfg.MinTrades+2 {
		out = append(out, ReasonLowSampleSize)
	}
	return out
}

// score returns a 0..100 heuristic for triage ranking. Not a probability —
// just a stable monotone-ish blend of "how far past the tier floor" the
// line is, capped to 100. Components:
//
//   - severity anchor (Info=20, Warning=40, Critical=60, Hard=80)
//   - up to 20 pts for total/floor ratio (saturates at 4×)
//   - up to 10 pts for market-multiplier excess (saturates at 4×)
//   - up to 5  pts for trader-multiplier excess (saturates at 4× of 10×
//     baseline ⇒ full points at 40× wallet-typical)
//   - up to 5  pts for lifecycle proximity to close (linear in %)
//
// Caps total at 100 even when all components max out.
func (d *Detector) score(line Line, t anomaly.Tier, sev anomaly.Severity) int {
	var s float64
	switch sev {
	case anomaly.SeverityHard:
		s += 80
	case anomaly.SeverityCritical:
		s += 60
	case anomaly.SeverityWarning:
		s += 40
	case anomaly.SeverityInfo:
		s += 20
	}
	if t.MinNotionalUSD > 0 {
		totalRatio := line.TotalNotionalUSD / (d.cfg.TotalMultiplier * t.MinNotionalUSD)
		s += saturate(totalRatio, 4) * 20
	}
	// v6 tail bonus replaces the median-multiplier bonus. Bigger weight
	// when both axes confirm than when only one does.
	if line.MarketP95USD > 0 && t.MinMarketP95Ratio > 0 {
		s += saturate(line.TotalNotionalUSD/line.MarketP95USD/t.MinMarketP95Ratio, 4) * 10
	}
	if line.TraderP95USD > 0 && t.MinTraderP95Ratio > 0 {
		s += saturate(line.TotalNotionalUSD/line.TraderP95USD/t.MinTraderP95Ratio, 4) * 5
	}
	s += math.Min(line.LifecyclePct/20, 5)
	if s > 100 {
		s = 100
	}
	if s < 0 {
		s = 0
	}
	return int(math.Round(s))
}

// confidence reflects sample-size and readiness — operators want to know
// "how much should I trust this number?". Components:
//
//   - 0.4 base when both market and trader baselines are ready
//   - 0.3 when only the market baseline is ready
//   - 0.2 when neither baseline is ready (rare; size path alone qualifies)
//   - + 0.3 × min(trades/10, 1) sample size bonus
//   - + 0.3 × min(span/4h, 1) span bonus (a 4h+ line is more credible
//     than a 5-minute spasm of the same total)
func (d *Detector) confidence(line Line) float64 {
	var c float64
	switch {
	case line.MarketP95USD > 0 && line.TraderP95USD > 0:
		c = 0.4
	case line.MarketP95USD > 0:
		c = 0.3
	default:
		c = 0.2
	}
	c += 0.3 * math.Min(float64(line.TradeCount)/10, 1)
	c += 0.3 * math.Min(line.Span().Hours()/4, 1)
	if c > 1 {
		c = 1
	}
	if c < 0 {
		c = 0
	}
	return c
}

// tierMinTrades is the per-tier line trade-count floor. Info=3, Warning=4,
// Critical=5. Hard is gated separately (in Decide) on lineTotal + HOT.
func tierMinTrades(sev anomaly.Severity) int {
	switch sev {
	case anomaly.SeverityCritical, anomaly.SeverityHard:
		return 5
	case anomaly.SeverityWarning:
		return 4
	default:
		return 3
	}
}

// saturate returns min(x/scale, 1) for x ≥ 0, 0 otherwise. Used in score
// to express "diminishing returns past a saturation point".
func saturate(x, scale float64) float64 {
	if x <= 0 || scale <= 0 {
		return 0
	}
	r := x / scale
	if r > 1 {
		return 1
	}
	return r
}
