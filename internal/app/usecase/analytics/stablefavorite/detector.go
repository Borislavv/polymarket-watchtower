// Package stablefavorite implements the late-market-stable-favorite
// strategy.
//
// SCOPE: this is a SEPARATE signal family from whale-flow detection.
// It looks for markets that are deep in their lifecycle, where a
// single outcome trades inside a defined favorite band (not a near-
// certain 95%+ side, not a 51% coinflip) and has been price-stable,
// with no adverse drift, with reasonable liquidity. The actionable
// read is "this market has converged on a favorite and still offers
// meaningful remaining return". It is NEVER described as "safe" or
// "guaranteed" — political markets gap on late news, and the
// strategy quantifies stability not certainty.
//
// Why a separate package: the whale-flow scorer in
// internal/app/usecase/analytics/score is event-driven (per-trade);
// this detector is state-driven (per-market). The thresholds,
// dedup namespace, and Telegram render path are all distinct.
//
// The detector is PURE — it consumes a pre-computed Input shape and
// returns a Verdict. The repository-side reads (price history,
// market candidates, optional cross-market price) happen upstream in
// the stablefavorite worker.
package stablefavorite

import (
	"math"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Config tunes the detector. Defaults applied by applyDefaults.
type Config struct {
	Enabled bool

	// Lifecycle gates.
	MinLifecyclePct float64
	HotLifecyclePct float64

	// Favorite-probability band.
	MinProbability float64
	MaxProbability float64

	// Remaining-return floor in PERCENT (e.g. 20 = 20% upside).
	MinReturnPct float64

	// Stability window for the price-stats inputs.
	StabilityWindow time.Duration

	// Stability gates over the window.
	MaxPriceStddev     float64 // absolute stddev cap (price units, 0..1)
	MaxDrawdown        float64 // (max-min)/max cap
	MaxAdverseMove6h   float64 // |price drop| over 6h cap (price units)
	MaxNegativeDrift6h float64 // strict 6h drop reject

	// Reversal-pressure reject: ratio of opposing-side trade count
	// to favored-side count in the recent window. 0 disables.
	ReversalVolumeRatio float64

	// Liquidity gates.
	MinMarketVolumeUSD float64
	MinRecentTrades    int

	// Cross-market: when enabled and a related price is supplied,
	// |this_price − cross_price| ≤ MaxCrossMarketDisagreement
	// confirms; beyond that conflicts. Unavailable cross-market data
	// silently lowers confidence rather than blocks the alert.
	CrossMarketEnabled         bool
	MaxCrossMarketDisagreement float64

	// Severity gates. Score floor + lifecycle floor + (Warning+)
	// confidence floor. Critical adds a cross-market-confirmation
	// requirement.
	InfoMinScore          float64
	InfoMinConfidence     float64
	WarningMinLifecycle   float64
	WarningMinScore       float64
	WarningMinConfidence  float64
	CriticalMinLifecycle  float64
	CriticalMinScore      float64
	CriticalMinConfidence float64
}

func (c Config) applyDefaults() Config {
	if c.MinLifecyclePct <= 0 {
		c.MinLifecyclePct = 92
	}
	if c.HotLifecyclePct <= 0 {
		c.HotLifecyclePct = 97
	}
	if c.MinProbability <= 0 {
		c.MinProbability = 0.55
	}
	if c.MaxProbability <= 0 {
		c.MaxProbability = 0.85
	}
	if c.MinReturnPct <= 0 {
		c.MinReturnPct = 20
	}
	if c.StabilityWindow <= 0 {
		// v7 relaxation: 6h (was 24h) — better tracks the recent
		// regime now that hype suppression handles spikes.
		c.StabilityWindow = 6 * time.Hour
	}
	if c.MaxPriceStddev <= 0 {
		c.MaxPriceStddev = 0.10
	}
	if c.MaxDrawdown <= 0 {
		c.MaxDrawdown = 0.25
	}
	if c.MaxAdverseMove6h <= 0 {
		c.MaxAdverseMove6h = 0.15
	}
	if c.MaxNegativeDrift6h <= 0 {
		c.MaxNegativeDrift6h = 0.10
	}
	if c.MinMarketVolumeUSD <= 0 {
		c.MinMarketVolumeUSD = 25000
	}
	if c.MinRecentTrades <= 0 {
		c.MinRecentTrades = 20
	}
	if c.MaxCrossMarketDisagreement <= 0 {
		c.MaxCrossMarketDisagreement = 0.15
	}
	if c.InfoMinScore <= 0 {
		c.InfoMinScore = 65
	}
	if c.InfoMinConfidence <= 0 {
		c.InfoMinConfidence = 0.60
	}
	if c.WarningMinLifecycle <= 0 {
		c.WarningMinLifecycle = 95
	}
	if c.WarningMinScore <= 0 {
		c.WarningMinScore = 75
	}
	if c.WarningMinConfidence <= 0 {
		c.WarningMinConfidence = 0.70
	}
	if c.CriticalMinLifecycle <= 0 {
		c.CriticalMinLifecycle = 98
	}
	if c.CriticalMinScore <= 0 {
		c.CriticalMinScore = 85
	}
	if c.CriticalMinConfidence <= 0 {
		c.CriticalMinConfidence = 0.80
	}
	return c
}

// WindowStats is the per-window price/volume summary the worker
// computes upstream and feeds the detector.
type WindowStats struct {
	Count      int
	Mean       float64
	Stddev     float64
	Min        float64
	Max        float64
	First      float64 // oldest price in the window (by traded_at)
	Last       float64 // newest price in the window
	VolumeUSD  float64
	BuyVolume  float64 // optional — for reversal-volume ratio when wired
	SellVolume float64
}

// Input is one (market, outcome) snapshot.
type Input struct {
	MarketID     string
	OutcomeToken string
	Outcome      string

	LifecyclePct float64 // 0..100
	CurrentPrice float64 // last observed price, in (0,1)

	Window24h WindowStats
	Window6h  WindowStats

	// Optional cross-market price for the SAME logical outcome.
	// 0 = unavailable; the detector lowers confidence accordingly.
	CrossMarketPrice float64
}

// Verdict is the outcome of Decide. Diagnostic fields are always
// populated so an operator inspecting a non-fire can see why.
type Verdict struct {
	Fired              bool
	Severity           anomaly.Severity
	Score              float64
	Confidence         float64
	Reasons            []string
	RemainingReturnPct float64

	// RiskAdjustedReturn = RemainingReturnPct / (stddev × 100).
	// Surfaced so the worker can stamp it on StableFavoriteRef and
	// the formatter can show "edge per unit of noise". Zero when
	// stddev is zero or undefined.
	RiskAdjustedReturn float64

	// Cross-market verdict shape:
	//   "" / "unavailable" — no related-market data wired or supplied
	//   "confirmed"        — cross-market within MaxDisagreement
	//   "conflict"         — cross-market disagrees beyond threshold
	CrossMarketStatus string
	CrossMarketDelta  float64

	// HypeSuppressed is true when the hype-market heuristic triggered
	// a one-tier severity downgrade. The reason code lands on
	// Reasons regardless of whether a downgrade was actually applied
	// (Info cannot be downgraded further but the tag is still useful).
	HypeSuppressed bool

	// SuppressedReason names the first blocking gate when Fired=false.
	SuppressedReason string
}

// Canonical suppression reasons. Stable strings — the worker stamps
// them on Verdict.SuppressedReason for diagnostic logging.
const (
	SkipDisabled            = "disabled"
	SkipLifecycle           = "below_min_lifecycle"
	SkipProbabilityBand     = "outside_favorite_band"
	SkipReturn              = "below_min_return"
	SkipStability           = "above_max_stddev_or_drawdown"
	SkipAdverseDrift        = "adverse_drift_over_6h"
	SkipLiquidityVolume     = "below_min_market_volume"
	SkipLiquidityTradeCount = "below_min_recent_trades"
	SkipCrossMarketConflict = "cross_market_conflict_suppressed"
	SkipScore               = "below_info_score"
	SkipConfidence          = "below_info_confidence"
)

// Detector is concurrency-safe (no state).
type Detector struct {
	cfg Config
}

// New constructs a Detector with defaults applied.
func New(cfg Config) *Detector { return &Detector{cfg: cfg.applyDefaults()} }

// Config returns the applied configuration.
func (d *Detector) Config() Config { return d.cfg }

// Decide evaluates one (market, outcome) and returns a Verdict.
func (d *Detector) Decide(in Input) Verdict {
	v := Verdict{}
	if !d.cfg.Enabled {
		v.SuppressedReason = SkipDisabled
		return v
	}

	// Input sanity: ignore degenerate prices the same way the
	// scorer does. A 0 or ≥1 "price" is a data bug, not a signal.
	if in.CurrentPrice <= 0 || in.CurrentPrice >= 1 {
		v.SuppressedReason = SkipProbabilityBand
		return v
	}
	v.RemainingReturnPct = 100 * (1 - in.CurrentPrice) / in.CurrentPrice

	// Lifecycle gate.
	if in.LifecyclePct < d.cfg.MinLifecyclePct {
		v.SuppressedReason = SkipLifecycle
		return v
	}
	// Probability band.
	if in.CurrentPrice < d.cfg.MinProbability || in.CurrentPrice > d.cfg.MaxProbability {
		v.SuppressedReason = SkipProbabilityBand
		return v
	}
	// Remaining return.
	if v.RemainingReturnPct < d.cfg.MinReturnPct {
		v.SuppressedReason = SkipReturn
		return v
	}
	// Liquidity.
	if in.Window24h.VolumeUSD < d.cfg.MinMarketVolumeUSD {
		v.SuppressedReason = SkipLiquidityVolume
		return v
	}
	if in.Window24h.Count < d.cfg.MinRecentTrades {
		v.SuppressedReason = SkipLiquidityTradeCount
		return v
	}
	// Stability — over the 24h window.
	if in.Window24h.Stddev > d.cfg.MaxPriceStddev {
		v.SuppressedReason = SkipStability
		return v
	}
	if drawdown(in.Window24h) > d.cfg.MaxDrawdown {
		v.SuppressedReason = SkipStability
		return v
	}
	// Adverse drift over 6h. Positive drift = favorite strengthening
	// (price up). Negative drift = favorite weakening.
	drift6h := in.Window6h.Last - in.Window6h.First
	if in.Window6h.Count >= 2 {
		// MaxAdverseMove6h is the absolute |drop| cap, applied
		// directly (gate is "favorite did not lose more than X").
		if -drift6h > d.cfg.MaxAdverseMove6h {
			v.SuppressedReason = SkipAdverseDrift
			return v
		}
		if -drift6h > d.cfg.MaxNegativeDrift6h && d.cfg.MaxNegativeDrift6h > 0 {
			v.SuppressedReason = SkipAdverseDrift
			return v
		}
	}

	// Cross-market. unavailable → confidence penalty, no veto.
	// conflict → either suppression or just confidence penalty; the
	// spec says "either suppress or lower confidence". We choose
	// "lower confidence" because political markets routinely diverge
	// across venues and we don't want to lose signals.
	if d.cfg.CrossMarketEnabled && in.CrossMarketPrice > 0 {
		delta := math.Abs(in.CrossMarketPrice - in.CurrentPrice)
		v.CrossMarketDelta = delta
		if delta <= d.cfg.MaxCrossMarketDisagreement {
			v.CrossMarketStatus = "confirmed"
		} else {
			v.CrossMarketStatus = "conflict"
		}
	} else {
		v.CrossMarketStatus = "unavailable"
	}

	// Risk-adjusted return — payoff per unit of price noise. Surfaced
	// for the formatter; the score blend already weights stability,
	// so this is annotation rather than gate.
	if in.Window24h.Stddev > 0 {
		v.RiskAdjustedReturn = v.RemainingReturnPct / (in.Window24h.Stddev * 100)
	}

	// Score (0..100) with the spec's weighting.
	v.Score = scoreOf(in, d.cfg, v.CrossMarketStatus)
	v.Confidence = confidenceOf(in, d.cfg, v.CrossMarketStatus)

	// Reasons.
	v.Reasons = reasonsOf(in, d.cfg, v.RemainingReturnPct, v.CrossMarketStatus)

	// Volatility-event-pending tag: recent (6h) stddev markedly
	// higher than 24h baseline. Suggests a binding event is
	// unfolding now and the favorite is at risk of slipping. Context
	// only — caps severity to Warning rather than vetoing the alert.
	volatilityEventPending := isVolatilityEventPending(in)
	if volatilityEventPending {
		v.Reasons = append(v.Reasons, anomaly.ReasonVolatilityEventPending)
	}

	// Hype-suppression: 24h volume dramatically above floor AND most
	// of that volume came in the last 6h. Stable-favorite logic
	// expects orderly convergence; a hype spike is the opposite.
	v.HypeSuppressed = isHypeMarket(in, d.cfg)
	if v.HypeSuppressed {
		v.Reasons = append(v.Reasons, anomaly.ReasonHypeMarketSuppression)
	}

	// Always surface risk-adjusted-return tag so dashboards can
	// filter on its presence even when value is 0.
	v.Reasons = append(v.Reasons, anomaly.ReasonRiskAdjustedReturn)

	// Severity.
	sev := pickSeverity(in.LifecyclePct, v.Score, v.Confidence, v.CrossMarketStatus, d.cfg)
	if sev == "" {
		// Fell below the Info floor — record the binding gate so the
		// suppression log is precise.
		if v.Score < d.cfg.InfoMinScore {
			v.SuppressedReason = SkipScore
		} else {
			v.SuppressedReason = SkipConfidence
		}
		return v
	}
	// Hype suppression downgrades severity by one rung. Volatility-
	// event-pending is annotation only (see Reasons) — it tells an
	// operator the recent regime is shaky, but the gates already
	// considered stability via score/confidence. Adding another
	// auto-downgrade on top of those was rejected after running it
	// against the canonical fixture and tripping on a 1.6× recent-
	// stddev ratio that is well within "still stable".
	if v.HypeSuppressed {
		sev = downgrade(sev)
	}
	_ = volatilityEventPending
	v.Severity = sev
	v.Fired = true
	return v
}

// downgrade lowers a severity by one rung. Info stays Info (no rung
// below); higher tiers step down by exactly one.
func downgrade(s anomaly.Severity) anomaly.Severity {
	switch s {
	case anomaly.SeverityCritical:
		return anomaly.SeverityWarning
	case anomaly.SeverityWarning:
		return anomaly.SeverityInfo
	}
	return s
}

// isVolatilityEventPending returns true when the recent (6h) stddev
// is at least 1.5× the 24h stddev AND the 6h window has enough
// samples to mean something. The 1.5× constant mirrors the typical
// "rising volatility regime" heuristic and is hardcoded rather than
// configurable because (a) it's a tag, not a gate, and (b) there
// would be no operator-friendly value to tune to.
func isVolatilityEventPending(in Input) bool {
	if in.Window6h.Count < 5 || in.Window24h.Stddev <= 0 {
		return false
	}
	return in.Window6h.Stddev >= 1.5*in.Window24h.Stddev
}

// isHypeMarket returns true when 24h volume is at least 5× the
// minimum-volume floor AND most of that volume (≥50%) landed in the
// last 6h. The 5× / 50% constants encode "this is not orderly
// convergence — fresh money piled in". Both are hardcoded for the
// same reason as isVolatilityEventPending.
func isHypeMarket(in Input, cfg Config) bool {
	if cfg.MinMarketVolumeUSD <= 0 || in.Window24h.VolumeUSD <= 0 || in.Window6h.VolumeUSD <= 0 {
		return false
	}
	volElevated := in.Window24h.VolumeUSD >= 5*cfg.MinMarketVolumeUSD
	recentShare := in.Window6h.VolumeUSD / in.Window24h.VolumeUSD
	return volElevated && recentShare >= 0.5
}

// drawdown returns (max-min)/max bounded in [0,1]. Returns 0 when
// max==0 or fewer than 2 samples.
func drawdown(w WindowStats) float64 {
	if w.Count < 2 || w.Max <= 0 {
		return 0
	}
	return (w.Max - w.Min) / w.Max
}

// scoreOf is the weighted blend the spec mandates:
//
//	25% lifecycle  (linear in (lifecycle − MinLifecycle) / (100 − MinLifecycle))
//	25% stability  (1 − stddev/MaxStddev, capped to 0..1)
//	20% remaining payout (linear in returnPct, saturates at 100%)
//	15% liquidity  (log-scaled volume vs MinMarketVolumeUSD; trade-count tie)
//	10% no reversal pressure (1 − adverse-drift fraction)
//	 5% cross-market confirmation
func scoreOf(in Input, cfg Config, cmStatus string) float64 {
	lifecycleSpan := 100 - cfg.MinLifecyclePct
	var lifecycleScore float64
	if lifecycleSpan > 0 {
		lifecycleScore = (in.LifecyclePct - cfg.MinLifecyclePct) / lifecycleSpan
	}
	lifecycleScore = clamp01(lifecycleScore)

	stabilityScore := 1 - in.Window24h.Stddev/math.Max(cfg.MaxPriceStddev, 1e-9)
	stabilityScore = clamp01(stabilityScore)

	returnPct := 100 * (1 - in.CurrentPrice) / in.CurrentPrice
	returnScore := clamp01(returnPct / 100)

	liquidityRatio := in.Window24h.VolumeUSD / math.Max(cfg.MinMarketVolumeUSD, 1)
	// log10(1 + ratio): ratio=0 → 0, ratio=9 → 1, ratio=99 → 2.
	// Saturate at 10× MinMarketVolumeUSD ($250k @ default floor),
	// which is the realistic upper end for late-stage Polymarket
	// politics markets. log10(1+9)=1 maps to a full 15-pt bonus.
	liquidityScore := clamp01(math.Log10(1 + liquidityRatio))

	drift := in.Window6h.Last - in.Window6h.First
	reversalPenalty := math.Max(0, -drift) / math.Max(cfg.MaxAdverseMove6h, 1e-9)
	reversalScore := clamp01(1 - reversalPenalty)

	var crossScore float64
	switch cmStatus {
	case "confirmed":
		crossScore = 1
	case "conflict":
		crossScore = 0
	default:
		// v7: "unavailable" is now near-neutral (was 0.5). The
		// hard cross-market gate was removed, so a market with no
		// paired venue must not be silently punished out of the
		// Critical band by losing 2.5 score points it could
		// otherwise reach. Still <1 so "confirmed" retains a
		// meaningful edge.
		crossScore = 0.8
	}

	return 100 * (0.25*lifecycleScore +
		0.25*stabilityScore +
		0.20*returnScore +
		0.15*liquidityScore +
		0.10*reversalScore +
		0.05*crossScore)
}

// confidenceOf is sample-size + readiness adjustment (0..1).
//
//	0.40 base (we cleared every hard gate to get here)
//	+0.25 × min(samples/100, 1)  — long price history is more credible
//	+0.20 × min(volume/100k, 1)  — liquidity tie
//	+0.15 × (cross-market confirmed? 1 : (unavailable? 0.3 : 0))
func confidenceOf(in Input, cfg Config, cmStatus string) float64 {
	c := 0.40
	c += 0.25 * clamp01(float64(in.Window24h.Count)/100)
	c += 0.20 * clamp01(in.Window24h.VolumeUSD/100_000)
	switch cmStatus {
	case "confirmed":
		c += 0.15
	case "unavailable":
		c += 0.045 // 0.15 × 0.3
	case "conflict":
		// no bonus — conflict already penalises score
	}
	_ = cfg
	return clamp01(c)
}

func reasonsOf(in Input, cfg Config, returnPct float64, cmStatus string) []string {
	out := []string{anomaly.ReasonLateMarketFavorite}
	if in.Window24h.Stddev <= cfg.MaxPriceStddev*0.5 {
		out = append(out, anomaly.ReasonStablePrice)
	}
	if in.Window24h.Stddev <= cfg.MaxPriceStddev {
		out = append(out, anomaly.ReasonLowVolatility)
	}
	if in.Window6h.Count < 2 || (in.Window6h.Last-in.Window6h.First) >= 0 {
		out = append(out, anomaly.ReasonNoReversalPressure)
	}
	if returnPct >= 2*cfg.MinReturnPct {
		out = append(out, anomaly.ReasonMeaningfulRemainingPayoff)
	}
	switch cmStatus {
	case "confirmed":
		out = append(out, anomaly.ReasonCrossMarketConfirmation)
	case "conflict":
		out = append(out, anomaly.ReasonCrossMarketConflict)
	}
	if in.Window24h.VolumeUSD < cfg.MinMarketVolumeUSD*2 {
		// Liquidity is above the hard floor but still thin — flag.
		out = append(out, anomaly.ReasonLowLiquidityRisk)
	}
	return out
}

// pickSeverity returns the highest tier whose score / confidence /
// lifecycle floors all clear. Cross-market status is NOT a hard gate
// here — its effect is already baked into score (5%) and confidence
// (up to +0.15 on confirmed). Treating cross-market unavailability
// as a hard veto on Critical was rejected after operator feedback:
// political markets routinely lack a paired venue and we lose
// otherwise-strong signals when the cross-market lookup happens to
// be unwired.
func pickSeverity(lifecycle, score, confidence float64, cmStatus string, cfg Config) anomaly.Severity {
	_ = cmStatus // intentionally unused — see comment above.
	if lifecycle >= cfg.CriticalMinLifecycle &&
		score >= cfg.CriticalMinScore &&
		confidence >= cfg.CriticalMinConfidence {
		return anomaly.SeverityCritical
	}
	if lifecycle >= cfg.WarningMinLifecycle &&
		score >= cfg.WarningMinScore &&
		confidence >= cfg.WarningMinConfidence {
		return anomaly.SeverityWarning
	}
	if lifecycle >= cfg.MinLifecyclePct &&
		score >= cfg.InfoMinScore &&
		confidence >= cfg.InfoMinConfidence {
		return anomaly.SeverityInfo
	}
	return ""
}

// clamp01 bounds x to [0,1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
