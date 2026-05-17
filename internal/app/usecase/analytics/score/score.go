// Package score evaluates one trade against a baseline and the configured
// single-trade thresholds, returning at most one anomaly result. It is pure
// (no I/O, no clock, no goroutines) and trivial to test exhaustively.
//
// A trade fires only when BOTH ladders qualify at Info or above:
//
//   - Absolute  : trade USD notional ≥ tier floor AND implied odds (1/price)
//     ≥ tier floor. Guards against tiny bets and against bets at near-even
//     odds where there's no asymmetric-payoff insider angle.
//   - Multiplier: trade USD notional / baseline median ≥ tier floor. Guards
//     against ordinary big bets on busy markets; the baseline must be material.
//
// Final severity is the lower (conservative) of the two — see
// anomaly.ConservativeMin. Either side below Info ⇒ no alert.
//
// Single-trade severity caps at Critical. HARD is reserved for the cluster
// detector (multiple sharks converging on one category).
package score

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Result is the outcome of scoring one trade.
type Result struct {
	Fired          bool
	Severity       anomaly.Severity // conservative-MIN of absolute and multiplier tiers
	AbsoluteTier   anomaly.Severity // tier crossed by (notional, odds)
	MultiplierTier anomaly.Severity // tier crossed by (notional / baseline median)
	Multiplier     float64          // observed notional / baseline median (0 if not evaluated)
	Odds           float64          // 1 / price
}

// Score evaluates the trade. price is the implied probability in (0, 1);
// odds = 1/price.
func Score(notionalUSD, price float64, bs baseline.Stats, t anomaly.Thresholds) Result {
	if notionalUSD <= 0 || price <= 0 {
		return Result{}
	}
	odds := 1.0 / price

	absoluteTier := t.AbsoluteTier(notionalUSD, odds)
	if absoluteTier == "" {
		return Result{Odds: odds}
	}

	// Multiplier path requires a meaningful baseline; without it we refuse
	// to rank rarity and the trade goes unalerted.
	if bs.Count < t.MinBaselineTrades || bs.TotalUSD < t.MinBaselineNotionalUSD || bs.MedianUSD <= 0 {
		return Result{Odds: odds, AbsoluteTier: absoluteTier}
	}
	multiplier := notionalUSD / bs.MedianUSD
	multiplierTier := t.MultiplierTier(multiplier)
	if multiplierTier == "" {
		return Result{Odds: odds, AbsoluteTier: absoluteTier, Multiplier: multiplier}
	}

	return Result{
		Fired:          true,
		Severity:       anomaly.ConservativeMin(absoluteTier, multiplierTier),
		AbsoluteTier:   absoluteTier,
		MultiplierTier: multiplierTier,
		Multiplier:     multiplier,
		Odds:           odds,
	}
}
