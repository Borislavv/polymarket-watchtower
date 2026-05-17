// Package score evaluates a single trade against a baseline and the configured
// thresholds, returning at most one anomaly result. It is pure (no I/O, no
// clock, no goroutines) — easy to test exhaustively.
//
// Two independent ladders are evaluated and the higher severity wins:
//
//  1. Multiplier ladder against the baseline median (skipped when the bucket's
//     sample count is below MinBaselineTrades — protects against the
//     "first trade looks like x1e6" false positive class that plagued the
//     previous aggregate-rate detector).
//  2. Absolute USD ladder against the trade's notional, evaluated regardless
//     of baseline. Catches whales on cold markets.
package score

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Result is the outcome of scoring one trade.
type Result struct {
	Fired        bool
	Severity     anomaly.Severity
	Reason       string  // "multiplier" | "absolute_tier" | "multiplier+absolute_tier"
	Multiplier   float64 // observed notional / baseline median (0 when not evaluated)
	AbsoluteTier float64 // crossed USD tier (0 when not evaluated)
}

// Score evaluates the trade. notionalUSD is the trade's USD value.
func Score(notionalUSD float64, bs baseline.Stats, t anomaly.Thresholds) Result {
	if notionalUSD <= 0 {
		return Result{}
	}
	var (
		out        Result
		mulSev     anomaly.Severity
		mulHit     bool
		absSev     anomaly.Severity
		absHit     bool
		multiplier float64
		absTier    float64
	)

	if bs.Count >= t.MinBaselineTrades && bs.MedianUSD > 0 && len(t.Multipliers) > 0 {
		multiplier = notionalUSD / bs.MedianUSD
		if sev, _, ok := anomaly.SeverityForLadder(multiplier, t.Multipliers); ok {
			mulSev, mulHit = sev, true
		}
	}
	if len(t.AbsoluteUSDTiers) > 0 {
		if sev, hit, ok := anomaly.SeverityForLadder(notionalUSD, t.AbsoluteUSDTiers); ok {
			absSev, absTier, absHit = sev, hit, true
		}
	}

	switch {
	case mulHit && absHit:
		out.Fired = true
		out.Severity = anomaly.MaxSeverity(mulSev, absSev)
		out.Multiplier = multiplier
		out.AbsoluteTier = absTier
		out.Reason = "multiplier+absolute_tier"
	case mulHit:
		out.Fired = true
		out.Severity = mulSev
		out.Multiplier = multiplier
		out.Reason = "multiplier"
	case absHit:
		out.Fired = true
		out.Severity = absSev
		out.AbsoluteTier = absTier
		out.Reason = "absolute_tier"
	}
	return out
}
