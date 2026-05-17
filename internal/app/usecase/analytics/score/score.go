// Package score evaluates one trade against a baseline and the configured
// single_cluster thresholds, returning at most one anomaly result. It is pure
// (no I/O, no clock, no goroutines) and trivial to test exhaustively.
//
// Three independent signals are evaluated; the higher severity wins:
//
//  1. Whale (relative): the trade's USD notional crosses a multiplier of the
//     baseline median for the same (category, market, outcome). Gated by
//     MinTradeUSD (no whale alerts on $5 bets) plus baseline-sample floors
//     (no "first trade looks like ∞×" false positives).
//
//  2. High odds: 1/price crosses an odds ladder. Catches the
//     asymmetric-payoff insider pattern (large bet on a long-odds leg) even
//     when no baseline exists. Gated by a softer notional floor so a $1
//     joke bet on a 0.001 outcome stays silent.
//
//  3. Combined: when both fire, the reason becomes HighOddsWhaleDetected —
//     the strongest single-trade signal we can emit.
package score

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Result is the outcome of scoring one trade.
type Result struct {
	Fired      bool
	Severity   anomaly.Severity
	Reason     string  // one of anomaly.Reason* constants when Fired
	Multiplier float64 // notional / baseline median; 0 when whale path did not run
	OddsRung   float64 // crossed odds-ladder rung; 0 when odds path did not fire
}

// Score evaluates the trade. price is the Polymarket implied probability of
// the outcome leg ((0,1)), so odds = 1/price (>=1).
func Score(notionalUSD, price float64, bs baseline.Stats, t anomaly.Thresholds) Result {
	if notionalUSD <= 0 || price <= 0 {
		return Result{}
	}
	odds := 1.0 / price

	var (
		whaleSev   anomaly.Severity
		whaleMul   float64
		whaleFired bool
	)
	if notionalUSD >= t.MinTradeUSD &&
		bs.Count >= t.MinBaselineTrades &&
		bs.TotalUSD >= t.MinBaselineNotionalUSD &&
		bs.MedianUSD > 0 &&
		len(t.MultiplierLadder) > 0 {
		whaleMul = notionalUSD / bs.MedianUSD
		if sev, _, ok := anomaly.SeverityForLadder(whaleMul, t.MultiplierLadder); ok {
			whaleSev, whaleFired = sev, true
		}
	}

	// Odds path uses a softer notional floor (MinTradeUSD/10) so small but
	// asymmetric bets are not silently dropped — they are exactly the signal
	// the spec wants ("Very high odds: meaningful notional with lower confidence").
	var (
		oddsSev   anomaly.Severity
		oddsHit   float64
		oddsFired bool
	)
	if len(t.OddsLadder) > 0 && notionalUSD >= t.MinTradeUSD/10 {
		if sev, hit, ok := anomaly.SeverityForLadder(odds, t.OddsLadder); ok {
			oddsSev, oddsHit, oddsFired = sev, hit, true
		}
	}

	switch {
	case whaleFired && oddsFired:
		return Result{
			Fired:      true,
			Severity:   anomaly.MaxSeverity(whaleSev, oddsSev),
			Reason:     anomaly.ReasonHighOddsWhale,
			Multiplier: whaleMul,
			OddsRung:   oddsHit,
		}
	case whaleFired:
		return Result{
			Fired:      true,
			Severity:   whaleSev,
			Reason:     anomaly.ReasonWhale,
			Multiplier: whaleMul,
		}
	case oddsFired:
		return Result{
			Fired:    true,
			Severity: oddsSev,
			Reason:   anomaly.ReasonHighOdds,
			OddsRung: oddsHit,
		}
	}
	return Result{}
}
