// Package score evaluates one trade against two baselines (market and
// trader-history) and the configured single-trade thresholds, returning at
// most one anomaly result. It is pure (no I/O, no clock, no goroutines) and
// trivial to test exhaustively.
//
// A trade fires only when BOTH ladders qualify at Info or above:
//
//   - Absolute  : trade USD notional ≥ tier floor AND implied odds (1/price)
//     ≥ tier floor. Guards against tiny bets and against bets at near-even
//     odds where there's no asymmetric-payoff insider angle.
//   - Multiplier: effective = max(marketMultiplier, traderMultiplier), where
//     marketMultiplier = notional / marketMedian (per-bucket) and
//     traderMultiplier = notional / traderMedian (over the wallet's history).
//     A trade qualifies if it is anomalous on EITHER axis — a small wallet
//     making an outsized bet is just as informative as a big bet on a quiet
//     market, and the surveillance literature treats them as complementary.
//
// Final severity is the lower (conservative) of the two ladders — see
// anomaly.ConservativeMin. Either side below Info ⇒ no alert. The absolute
// ladder remains the spam filter; the multiplier ladder is the rarity filter.
//
// Readiness is the caller's responsibility: pass an empty baseline.Stats
// (Count=0 or MedianUSD<=0) to disable that axis. The detector enforces
// MinBaselineTrades, MinBaselineNotionalUSD, BaselineMinReadySpan on the
// market side and MinTraderHistoryTrades on the trader side before calling
// Score; this package does NOT re-check those gates.
//
// Single-trade severity caps at Critical. HARD is reserved for the cluster
// detector (multiple sharks converging on one category).
package score

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// MultiplierAxis names which baseline contributed the effective multiplier.
// Surfaced in the alert so operators can tell "this fired because the wallet
// is small" vs "this fired because the market is quiet" at a glance.
type MultiplierAxis string

const (
	MultiplierAxisNone   MultiplierAxis = ""
	MultiplierAxisMarket MultiplierAxis = "market"
	MultiplierAxisTrader MultiplierAxis = "trader"
	MultiplierAxisBoth   MultiplierAxis = "both"
)

// Result is the outcome of scoring one trade.
type Result struct {
	Fired               bool
	Severity            anomaly.Severity // conservative-MIN of absolute and multiplier tiers
	AbsoluteTier        anomaly.Severity // tier crossed by (notional, odds)
	MultiplierTier      anomaly.Severity // tier crossed by effective multiplier
	MarketMultiplier    float64          // notional / market-median (0 if market leg not ready)
	TraderMultiplier    float64          // notional / trader-median (0 if trader leg not ready)
	EffectiveMultiplier float64          // max(market, trader) — what the multiplier tier was evaluated on
	MultiplierAxis      MultiplierAxis   // which axis contributed the effective multiplier
	Odds                float64          // 1 / price
}

// Score evaluates the trade.
//
//   - price is the implied probability in (0, 1); odds = 1/price.
//   - market is the per-bucket distribution. Pass baseline.Stats{} to disable
//     the market axis (caller's readiness gate failed).
//   - trader is the wallet's full-history distribution. Pass baseline.Stats{}
//     to disable the trader axis (wallet too new, or trader unknown).
//
// Both axes can be disabled simultaneously: in that case only the absolute
// tier is computed and no fire is possible. The function is pure.
func Score(notionalUSD, price float64, market, trader baseline.Stats, t anomaly.Thresholds) Result {
	if notionalUSD <= 0 || price <= 0 {
		return Result{}
	}
	odds := 1.0 / price

	absoluteTier := t.AbsoluteTier(notionalUSD, odds)
	if absoluteTier == "" {
		// Spam filter rejected — no need to evaluate the rarity filter.
		return Result{Odds: odds}
	}

	marketMultiplier := multiplierFor(notionalUSD, market, t)
	traderMultiplier := multiplierFor(notionalUSD, trader, t)

	effective, axis := pickEffective(marketMultiplier, traderMultiplier)
	if effective <= 0 {
		// Both axes disabled or no median yet — preserve the v1 contract of
		// refusing to rank rarity without a baseline.
		return Result{
			Odds:             odds,
			AbsoluteTier:     absoluteTier,
			MarketMultiplier: marketMultiplier,
			TraderMultiplier: traderMultiplier,
		}
	}
	multiplierTier := t.MultiplierTier(effective)
	if multiplierTier == "" {
		return Result{
			Odds:                odds,
			AbsoluteTier:        absoluteTier,
			MarketMultiplier:    marketMultiplier,
			TraderMultiplier:    traderMultiplier,
			EffectiveMultiplier: effective,
			MultiplierAxis:      axis,
		}
	}

	return Result{
		Fired:               true,
		Severity:            anomaly.ConservativeMin(absoluteTier, multiplierTier),
		AbsoluteTier:        absoluteTier,
		MultiplierTier:      multiplierTier,
		MarketMultiplier:    marketMultiplier,
		TraderMultiplier:    traderMultiplier,
		EffectiveMultiplier: effective,
		MultiplierAxis:      axis,
		Odds:                odds,
	}
}

// multiplierFor returns notional / median when the supplied baseline clears
// the count + total + median sanity floors. Returns 0 to signal "axis not
// ready"; callers treat 0 as "this leg does not contribute".
//
// Readiness for the count/total floors is shared between market and trader
// legs by design — a one-trade baseline gives a one-trade median and is too
// noisy to rank rarity from, regardless of whose history it is.
func multiplierFor(notionalUSD float64, bs baseline.Stats, t anomaly.Thresholds) float64 {
	if bs.Count < t.MinBaselineTrades || bs.TotalUSD < t.MinBaselineNotionalUSD || bs.MedianUSD <= 0 {
		return 0
	}
	return notionalUSD / bs.MedianUSD
}

// pickEffective returns the higher of the two multipliers and names the
// contributing axis. Equal & both non-zero → axis="both" (the trade is an
// outlier on both axes, which is the strongest single-trade evidence).
func pickEffective(market, trader float64) (float64, MultiplierAxis) {
	switch {
	case market <= 0 && trader <= 0:
		return 0, MultiplierAxisNone
	case trader <= 0:
		return market, MultiplierAxisMarket
	case market <= 0:
		return trader, MultiplierAxisTrader
	case market > trader:
		return market, MultiplierAxisMarket
	case trader > market:
		return trader, MultiplierAxisTrader
	default: // equal, both > 0
		return market, MultiplierAxisBoth
	}
}
