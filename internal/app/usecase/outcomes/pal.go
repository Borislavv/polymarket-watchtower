package outcomes

import (
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// PAL = Proof of Alert Value. The metrics in this file are the
// signal-quality measurement layer: directional success alone is
// vanity, but realized edge, weighted success, and calibration are
// decision-useful. They are computed at outcome-classification time
// from data already present on the alert row (price, side, severity,
// kind) and the resolution verdict — no new SQL, no new persistence.
//
// Vocabulary discipline:
//   - "realized edge"   = directional_success - implied_probability
//   - "implied probability" = the trade-time price, adjusted for
//                              side (a SELL YES at price 0.10 implies
//                              the wallet believes NO has 0.90 odds
//                              of winning, so the implied probability
//                              of the wallet's prediction is 0.90)
//   - "weighted success" = directional_success × severity_weight
//   - "calibration bucket" = grouping of alerts by implied probability
//
// None of these prove insider trading. They prove (or fail to prove)
// that the watchtower's alerts beat the implied probability the
// market itself was pricing — which is the only honest definition of
// "did the alert have value?"

// ImpliedProbability returns the alert's implied probability of being
// directionally correct, given the trade price and side at alert
// time. For a BUY at price p, the wallet is betting the YES side at
// market-priced probability p. For a SELL at price p, the wallet is
// betting against YES — its prediction (NO wins) has implied
// probability (1-p).
//
// price is clamped to [0,1] defensively; an out-of-range input would
// produce nonsensical edges otherwise.
func ImpliedProbability(price float64, side trade.Side) float64 {
	if price < 0 {
		price = 0
	}
	if price > 1 {
		price = 1
	}
	if side == trade.SideSell {
		return 1 - price
	}
	return price
}

// RealizedEdge returns (edge, true) when the outcome is in a state
// that admits an edge calculation (resolved_correct or resolved_wrong).
// pending / unknown / unavailable return (0, false) — the caller must
// not include those in the edge metric.
//
// Edge math:
//
//	resolved_correct  → success_binary = 1 → edge = 1 - implied_prob
//	resolved_wrong    → success_binary = 0 → edge = 0 - implied_prob = -implied_prob
//
// Examples (BUY YES at price p, where p is implied_prob):
//
//	BUY YES @ 0.50, YES wins → edge = +0.50
//	BUY YES @ 0.50, NO  wins → edge = -0.50
//	BUY YES @ 0.10, YES wins → edge = +0.90    (huge edge on a long-shot)
//	BUY YES @ 0.10, NO  wins → edge = -0.10    (small loss on a long-shot)
//	BUY YES @ 0.90, YES wins → edge = +0.10    (small edge on a chalk)
//	BUY YES @ 0.90, NO  wins → edge = -0.90    (large loss on a chalk that broke)
func RealizedEdge(status repository.OutcomeStatus, impliedProb float64) (float64, bool) {
	switch status {
	case repository.OutcomeCorrect:
		return 1.0 - impliedProb, true
	case repository.OutcomeWrong:
		return 0.0 - impliedProb, true
	default:
		return 0, false
	}
}

// CalibrationBucket maps an implied probability to the canonical
// label used in the watchtower_alert_calibration_total metric.
// Bucket cutoffs are chosen so the lower band (0-30%) — where
// informed-flow signal should show up most clearly — gets fine
// granularity, while the upper band is coarser (chalk plays are
// less informative).
func CalibrationBucket(impliedProb float64) string {
	switch {
	case impliedProb < 0.10:
		return "0-10"
	case impliedProb < 0.20:
		return "10-20"
	case impliedProb < 0.30:
		return "20-30"
	case impliedProb < 0.40:
		return "30-40"
	case impliedProb < 0.50:
		return "40-50"
	case impliedProb < 0.70:
		return "50-70"
	default:
		return "70+"
	}
}

// SeverityWeight maps a severity string to the multiplier used in
// weighted-success metrics. Conservative defaults:
//
//	Info     = 1     (background validation signal)
//	Warning  = 3     (page-someone-after-coffee band)
//	Critical = 10    (wake-up band)
//	Hard     = 25    (cluster — multiple wallets converging)
//
// Unknown / empty severities map to 0 so a misconfigured emission
// can't accidentally inflate the weighted total. The weights are
// surfaced as a constant rather than env-tunable because changing
// them retroactively changes historical metrics — operators who want
// a different weighting should compute it in PromQL.
func SeverityWeight(severity string) float64 {
	switch severity {
	case "info":
		return 1
	case "warning":
		return 3
	case "critical":
		return 10
	case "hard":
		return 25
	default:
		return 0
	}
}

// PALSnapshot is the projection a single classified alert contributes
// to the PAL metrics. Pure data — the caller increments counters.
type PALSnapshot struct {
	Edge          float64
	EdgeValid     bool
	ImpliedProb   float64
	SuccessBinary float64 // 0 or 1, only meaningful when EdgeValid
	Weight        float64
	Bucket        string
	Status        repository.OutcomeStatus
	Severity      string
	Kind          string
}

// BuildSnapshot constructs the PAL projection from the classified
// outcome plus the trade-time price and side. Returns EdgeValid=false
// for any non-resolved verdict so the caller can skip the edge /
// weighted metrics for those rows (they still get the calibration
// counter because "how many low-probability alerts went pending" is
// useful for sample-size diagnosis).
func BuildSnapshot(
	status repository.OutcomeStatus,
	severity, kind string,
	price float64,
	side trade.Side,
) PALSnapshot {
	imp := ImpliedProbability(price, side)
	snap := PALSnapshot{
		ImpliedProb: imp,
		Bucket:      CalibrationBucket(imp),
		Weight:      SeverityWeight(severity),
		Status:      status,
		Severity:    severity,
		Kind:        kind,
	}
	if edge, ok := RealizedEdge(status, imp); ok {
		snap.Edge = edge
		snap.EdgeValid = true
		if status == repository.OutcomeCorrect {
			snap.SuccessBinary = 1
		}
	}
	return snap
}
