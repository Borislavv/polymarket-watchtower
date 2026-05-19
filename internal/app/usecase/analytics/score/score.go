// Package score evaluates one trade against the market and trader-history
// distributions and the configured single-trade thresholds. It is pure (no
// I/O, no clock, no goroutines) and trivial to test exhaustively.
//
// Strategy v5 — tail + payoff
//
// A trade fires at the highest tier (Critical → Warning → Info) whose every
// configured gate clears:
//
//  1. Absolute floors: notional ≥ tier.MinNotionalUSD AND odds ≥ tier.MinOdds.
//     Pure spam filter, retained from v4.
//  2. Payoff floor: profitIfWin = notional × (odds − 1) ≥ tier.MinProfitUSD.
//     Filters "big bet at fair odds" — a $100k stake at 1.05× pays
//     $5k if it wins, which is not insider-grade. 0 disables.
//  3. Market tail (enforced only when the market baseline is ready):
//     notional/marketP95 ≥ tier.MinMarketP95Ratio AND
//     notional/marketP99 ≥ tier.MinMarketP99Ratio (each 0 disables).
//  4. Trader tail (enforced only when the trader baseline is ready):
//     notional/traderP95 ≥ tier.MinTraderP95Ratio AND
//     notional/traderP99 ≥ tier.MinTraderP99Ratio (each 0 disables).
//
// Median multipliers are not deciding gates anymore — they were the v4
// false-positive source (a $2k bet on a quiet outcome sits 300× above the
// $6 median while still being below the wallet's own typical trade). They
// remain accessible via baseline.Stats.MedianUSD for display only.
//
// Readiness rules:
//   - Market ready: market.Count ≥ t.MinBaselineTrades AND
//     market.TotalUSD ≥ t.MinBaselineNotionalUSD AND market.P95USD > 0.
//   - Trader ready: trader.Count ≥ t.MinBaselineTrades AND trader.P95USD > 0.
//     The detector enforces its own additional MinTraderHistoryTrades gate
//     before calling Score (zero-stats Stats{} disables this axis cleanly).
//
// Single-trade severity caps at Critical. HARD is reserved for the cluster
// detector (multiple sharks converging on one category).
package score

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Suppression reasons surfaced on Result.SuppressedReason when Fired=false.
// Operators see these in the structured detector log even though Telegram
// never receives a "would-have-fired" message.
const (
	SuppressedBelowAbsolute  = "below_min_notional_or_odds"
	SuppressedBelowPayoff    = "below_min_profit"
	SuppressedBelowMarketP95 = "below_market_p95_ratio"
	SuppressedBelowMarketP99 = "below_market_p99_ratio"
	SuppressedBelowTraderP95 = "below_trader_p95_ratio"
	SuppressedBelowTraderP99 = "below_trader_p99_ratio"
	SuppressedInvalidInput   = "invalid_input"
)

// Result is the outcome of scoring one trade. Even when Fired=false the
// payoff and ratio fields are populated so a debug/diagnostic caller can
// inspect why the trade was suppressed.
type Result struct {
	Fired    bool
	Severity anomaly.Severity

	Odds float64

	GrossPayoutIfWinUSD float64
	ProfitIfWinUSD      float64

	MarketP95Ratio float64
	MarketP99Ratio float64
	TraderP95Ratio float64
	TraderP99Ratio float64

	// PayoffGatePassed reflects whether the profit floor of the firing
	// tier (or the Info tier, when no tier fired) was satisfied.
	PayoffGatePassed bool
	// TailGatePassed reflects whether the firing tier's tail gates were
	// satisfied. When neither baseline was ready, the gates are
	// unenforceable and TailGatePassed=false even on a firing trade.
	TailGatePassed bool

	// LowMarketBaselineConfidence is true when the market baseline was
	// not ready (Count/TotalUSD/P95 readiness floor failed). The
	// firing decision still respected the gate (left unenforced), but
	// the alert payload should warn the operator that the per-bucket
	// rarity dimension is missing.
	LowMarketBaselineConfidence bool
	// LowTraderBaselineConfidence is the analogous flag for the wallet
	// distribution.
	LowTraderBaselineConfidence bool
	// SeverityCapped is true when the firing severity was lowered
	// because of LowMarketBaselineConfidence. The Telegram formatter
	// surfaces this as a "severity reduced — thin baseline" line.
	SeverityCapped bool

	// SuppressedReason names the first blocking gate when Fired=false.
	// Empty when Fired=true or when the input was invalid.
	SuppressedReason string
}

// Score evaluates the trade.
//
//   - price is the implied probability in (0, 1); odds = 1/price.
//   - market is the per-bucket distribution; pass baseline.Stats{} to mark
//     the market baseline unavailable.
//   - trader is the wallet's full-history distribution; pass baseline.Stats{}
//     to mark the trader axis unavailable.
//
// The function is pure.
func Score(notionalUSD, price float64, market, trader baseline.Stats, t anomaly.Thresholds) Result {
	if notionalUSD <= 0 || price <= 0 || price >= 1 {
		return Result{SuppressedReason: SuppressedInvalidInput}
	}
	odds := 1.0 / price

	base := Result{
		Odds:                odds,
		GrossPayoutIfWinUSD: notionalUSD * odds,
		ProfitIfWinUSD:      notionalUSD * (odds - 1),
		MarketP95Ratio:      safeRatio(notionalUSD, market.P95USD),
		MarketP99Ratio:      safeRatio(notionalUSD, market.P99USD),
		TraderP95Ratio:      safeRatio(notionalUSD, trader.P95USD),
		TraderP99Ratio:      safeRatio(notionalUSD, trader.P99USD),
	}

	marketReady := market.Count >= t.MinBaselineTrades &&
		market.TotalUSD >= t.MinBaselineNotionalUSD &&
		market.P95USD > 0
	// Trader axis does not gate on TotalUSD: a small wallet's p95 is
	// still meaningful at low aggregate USD. The detector layer applies
	// MinTraderHistoryTrades upstream of Score.
	traderReady := trader.Count >= t.MinBaselineTrades && trader.P95USD > 0

	base.LowMarketBaselineConfidence = !marketReady
	base.LowTraderBaselineConfidence = !traderReady

	for _, tier := range []struct {
		sev  anomaly.Severity
		spec anomaly.Tier
	}{
		{anomaly.SeverityCritical, t.Critical},
		{anomaly.SeverityWarning, t.Warning},
		{anomaly.SeverityInfo, t.Info},
	} {
		v := evaluateTier(notionalUSD, odds, base.ProfitIfWinUSD,
			base.MarketP95Ratio, base.MarketP99Ratio,
			base.TraderP95Ratio, base.TraderP99Ratio,
			marketReady, traderReady, tier.spec)
		if v.passed {
			r := base
			r.Fired = true
			r.Severity = tier.sev
			r.PayoffGatePassed = v.payoffPassed
			r.TailGatePassed = v.tailPassed
			applyLowBaselineCap(&r, tier.sev, notionalUSD, odds, t)
			return r
		}
	}

	// No tier passed — report the strongest blocking gate at the Info
	// tier (the easiest rung) so operators can see what's holding fires
	// back. evaluateTier returns the first failing gate; at Info that's
	// the most lenient evaluation we did.
	v := evaluateTier(notionalUSD, odds, base.ProfitIfWinUSD,
		base.MarketP95Ratio, base.MarketP99Ratio,
		base.TraderP95Ratio, base.TraderP99Ratio,
		marketReady, traderReady, t.Info)
	base.SuppressedReason = v.suppression
	base.PayoffGatePassed = v.payoffPassed
	return base
}

type tierVerdict struct {
	passed       bool
	payoffPassed bool
	tailPassed   bool
	suppression  string
}

// evaluateTier checks every configured gate for a single tier in a fixed
// order: absolute → payoff → market tail → trader tail. Returns the first
// blocking gate's suppression code when any fails.
func evaluateTier(
	notional, odds, profit, mp95, mp99, tp95, tp99 float64,
	marketReady, traderReady bool,
	tier anomaly.Tier,
) tierVerdict {
	if notional < tier.MinNotionalUSD || (tier.MinOdds > 0 && odds < tier.MinOdds) {
		return tierVerdict{suppression: SuppressedBelowAbsolute}
	}

	payoffPassed := tier.MinProfitUSD <= 0 || profit >= tier.MinProfitUSD
	if !payoffPassed {
		return tierVerdict{suppression: SuppressedBelowPayoff}
	}

	// Tail gates are only meaningful when their baseline is ready.
	// Count enforced gates so TailGatePassed reflects "at least one tail
	// dimension actually constrained the trade".
	enforcedTail := 0
	if marketReady {
		if tier.MinMarketP95Ratio > 0 {
			if mp95 < tier.MinMarketP95Ratio {
				return tierVerdict{payoffPassed: true, suppression: SuppressedBelowMarketP95}
			}
			enforcedTail++
		}
		if tier.MinMarketP99Ratio > 0 {
			if mp99 < tier.MinMarketP99Ratio {
				return tierVerdict{payoffPassed: true, suppression: SuppressedBelowMarketP99}
			}
			enforcedTail++
		}
	}
	if traderReady {
		if tier.MinTraderP95Ratio > 0 {
			if tp95 < tier.MinTraderP95Ratio {
				return tierVerdict{payoffPassed: true, suppression: SuppressedBelowTraderP95}
			}
			enforcedTail++
		}
		if tier.MinTraderP99Ratio > 0 {
			if tp99 < tier.MinTraderP99Ratio {
				return tierVerdict{payoffPassed: true, suppression: SuppressedBelowTraderP99}
			}
			enforcedTail++
		}
	}

	return tierVerdict{passed: true, payoffPassed: payoffPassed, tailPassed: enforcedTail > 0}
}

// applyLowBaselineCap implements the v6 severity-cap rule: when the
// market baseline is not ready the trade fired through an unenforced
// market tail gate, so its severity must be capped at
// Thresholds.LowBaselineSingleMaxSeverity (typically Info) unless the
// trade clears the Critical absolute floor AND
// Thresholds.LowBaselineAllowCriticalAbsolute is true.
//
// The trader baseline doesn't trigger a cap on its own — a wallet we
// don't know is fine to alert on if the market tail is solid. A cap
// flag is still set on the Result for the alert payload.
func applyLowBaselineCap(r *Result, fired anomaly.Severity, notional, odds float64, t anomaly.Thresholds) {
	if !t.LowBaselineCapEnabled {
		return
	}
	if !r.LowMarketBaselineConfidence {
		return
	}
	cap := t.LowBaselineSingleMaxSeverity
	if cap == "" {
		cap = anomaly.SeverityInfo
	}
	// Allowed Critical-absolute exception: notional and odds clear the
	// Critical absolute floors (already gated by the tier evaluator —
	// if we fired at Critical the trade passed).
	if t.LowBaselineAllowCriticalAbsolute &&
		notional >= t.Critical.MinNotionalUSD &&
		(t.Critical.MinOdds <= 0 || odds >= t.Critical.MinOdds) {
		return
	}
	// Only cap downward — never escalate.
	if severityRank(fired) > severityRank(cap) {
		r.Severity = cap
		r.SeverityCapped = true
	}
}

// severityRank is the same ordering anomaly.rank uses; copied here to
// avoid importing private helpers. Hard > Critical > Warning > Info.
func severityRank(s anomaly.Severity) int {
	switch s {
	case anomaly.SeverityHard:
		return 4
	case anomaly.SeverityCritical:
		return 3
	case anomaly.SeverityWarning:
		return 2
	case anomaly.SeverityInfo:
		return 1
	}
	return 0
}

// safeRatio guards against division by zero. Returns 0 when the divisor is
// non-positive — callers treat 0 as "unavailable", which matches the
// "baseline not ready" semantics.
func safeRatio(num, denom float64) float64 {
	if denom <= 0 {
		return 0
	}
	return num / denom
}
