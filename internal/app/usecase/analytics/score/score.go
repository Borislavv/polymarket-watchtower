// Package score evaluates one trade against a baseline and the configured
// single_cluster thresholds, returning at most one anomaly result. It is pure
// (no I/O, no clock, no goroutines) and trivial to test exhaustively.
//
// A trade fires only when BOTH ladders qualify at info or above:
//   - Absolute (notional AND odds) — guards against tiny bets and against bets
//     at near-even odds where there's no asymmetric-payoff insider angle.
//   - Multiplier (notional / baseline median) — guards against ordinary big
//     bets on busy markets; the baseline must already be material.
//
// Final severity is the lower (conservative) of the two — see
// anomaly.ConservativeMin. Either side below info ⇒ no alert.
package score

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Result is the outcome of scoring one trade.
type Result struct {
	Fired          bool
	Severity       anomaly.Severity // conservative MIN of absolute and multiplier tiers
	AbsoluteTier   anomaly.Severity // tier crossed by (notional, odds)
	MultiplierTier anomaly.Severity // tier crossed by (notional / baseline median)
	Multiplier     float64          // observed notional / baseline median (0 if not evaluated)
	Odds           float64          // 1/price
}

// Score evaluates the trade. price is the implied probability (0,1); odds = 1/price.
func Score(notionalUSD, price float64, bs baseline.Stats, t anomaly.Thresholds) Result {
	if notionalUSD <= 0 || price <= 0 {
		return Result{}
	}
	odds := 1.0 / price

	absTier := t.AbsoluteTier(notionalUSD, odds)
	if absTier == "" {
		return Result{Odds: odds}
	}

	// Multiplier path requires a meaningful baseline; without it we refuse to
	// rank rarity and the trade goes unalerted (spec: "If baseline is
	// insufficient, do not produce normal alert").
	if bs.Count < t.MinBaselineTrades || bs.TotalUSD < t.MinBaselineNotionalUSD || bs.MedianUSD <= 0 {
		return Result{Odds: odds, AbsoluteTier: absTier}
	}
	mul := notionalUSD / bs.MedianUSD
	mulTier := t.MultiplierTier(mul)
	if mulTier == "" {
		return Result{Odds: odds, AbsoluteTier: absTier, Multiplier: mul}
	}

	finalSev := anomaly.ConservativeMin(absTier, mulTier)
	// Overrides stack from softest to hardest so the final pick is the
	// strongest signal that fired.
	if t.MeetsHugeWhale(notionalUSD, odds, mul) && anomaly.RankAtLeast(finalSev, anomaly.SeverityCritical) == "" {
		finalSev = anomaly.SeverityCritical
	}
	if t.MeetsHardPromotion(notionalUSD, odds, mul) || t.MeetsMegaWhale(notionalUSD, odds, mul) {
		finalSev = anomaly.SeverityHard
	}
	return Result{
		Fired:          true,
		Severity:       finalSev,
		AbsoluteTier:   absTier,
		MultiplierTier: mulTier,
		Multiplier:     mul,
		Odds:           odds,
	}
}
