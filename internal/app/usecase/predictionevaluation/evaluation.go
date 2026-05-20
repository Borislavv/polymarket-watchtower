// Package predictionevaluation classifies a prediction-feedback row
// into one of 8 deterministic outcome buckets the operator can read.
// No AI — the inputs are the prediction row + its feedback row(s) +
// the live state machine fields (catalyst status, repricing status).
//
// The Classify function is pure; the worker that calls it batches
// over feedback rows and writes one polymarket_prediction_evaluations
// row per (prediction_id, horizon). Re-runs are idempotent.
package predictionevaluation

import (
	"encoding/json"
	"math"
	"strings"
)

// Class is the closed enum the migration's CHECK constraint
// enforces. Stable IDs so the dashboard panel can chart them.
type Class string

const (
	ClassUsefulCorrect      Class = "useful_correct"
	ClassUsefulEarly        Class = "useful_early"
	ClassCorrectButLate     Class = "correct_but_late"
	ClassStaleNoMove        Class = "stale_no_move"
	ClassWrongDirection     Class = "wrong_direction"
	ClassAlreadyPricedNoise Class = "already_priced_noise"
	ClassBlockedUnresolved  Class = "blocked_unresolved"
	ClassInsufficientData   Class = "insufficient_data"
)

// Inputs are the deterministic facts the classifier needs. The
// caller pulls them from polymarket_prediction_feedback +
// polymarket_market_predictions + polymarket_repricing_signals.
type Inputs struct {
	Horizon                  string // "1h" | "6h" | "24h"
	SideBias                 string // "bullish" | "bearish" | "neutral"
	PriceAtPrediction        *float64
	PriceAtHorizon           *float64
	UsefulnessAtCreation     float64 // 0..1 — usefulness score at the time the prediction was created
	StateAtHorizon           string  // current state field on the prediction at horizon-time
	RepricingStatusAtHorizon string  // "underreacting"|"already_priced"|"reversed"|"unclear"|...
	FlowConfirmed            bool    // worker computed this from same-side flow majority
	CatalystResolved         bool    // any catalyst on the event resolved by the horizon
	// MinMaterialDelta is the configurable price-move floor below
	// which we treat a horizon as "no movement" rather than
	// "direction correct/wrong". Default 0.03 (3% of probability).
	MinMaterialDelta float64
	// UsefulEarlyWindow is the horizon label (e.g. "6h") under
	// which a correct call is "useful_early" rather than
	// "useful_correct".
	UsefulEarlyWindow string
}

// Decision is the classifier output. Score is a 0..1 internal rank
// (higher = more useful) that the dashboard panel uses to sort.
// Evidence carries the raw inputs as JSON so the operator can audit
// without rejoining tables.
type Decision struct {
	Class    Class
	Score    float64
	Evidence []byte
}

// Classify returns the deterministic evaluation. The rules in
// priority order:
//
//  1. insufficient_data — missing prices.
//  2. blocked_unresolved — state=blocked at horizon AND no
//     resolved catalyst yet.
//  3. wrong_direction — delta sign opposite to side_bias and
//     |delta| ≥ MinMaterialDelta.
//  4. stale_no_move — |delta| < MinMaterialDelta AND state=stale.
//  5. already_priced_noise — |delta| < MinMaterialDelta AND
//     repricing=already_priced.
//  6. correct_but_late — delta in the right direction BUT
//     repricing=already_priced (market moved before our window).
//  7. useful_early — correct direction AND horizon ≤ UsefulEarlyWindow.
//  8. useful_correct — everything else with correct direction.
//
// Neutral side_bias predictions are bucketed by repricing/state
// only — direction is undefined.
func Classify(in Inputs) Decision {
	min := in.MinMaterialDelta
	if min <= 0 {
		min = 0.03
	}
	ev := map[string]any{
		"horizon":            in.Horizon,
		"side":               in.SideBias,
		"state":              in.StateAtHorizon,
		"repricing":          in.RepricingStatusAtHorizon,
		"flow_confirmed":     in.FlowConfirmed,
		"catalyst_resolved":  in.CatalystResolved,
		"min_material_delta": min,
	}
	// 1) insufficient_data
	if in.PriceAtPrediction == nil || in.PriceAtHorizon == nil {
		return Decision{Class: ClassInsufficientData, Score: 0, Evidence: marshal(ev)}
	}
	delta := *in.PriceAtHorizon - *in.PriceAtPrediction
	ev["price_at_prediction"] = *in.PriceAtPrediction
	ev["price_at_horizon"] = *in.PriceAtHorizon
	ev["delta"] = delta

	// 2) blocked_unresolved
	if in.StateAtHorizon == "blocked" && !in.CatalystResolved {
		return Decision{Class: ClassBlockedUnresolved, Score: 0.2, Evidence: marshal(ev)}
	}

	side := strings.ToLower(in.SideBias)
	absDelta := math.Abs(delta)
	dirSign := signOf(delta)
	expectedSign := 0.0
	switch side {
	case "bullish":
		expectedSign = 1
	case "bearish":
		expectedSign = -1
	}
	directionCorrect := expectedSign != 0 && dirSign == expectedSign
	directionWrong := expectedSign != 0 && dirSign != 0 && dirSign != expectedSign
	repricing := strings.ToLower(in.RepricingStatusAtHorizon)
	state := strings.ToLower(in.StateAtHorizon)

	// 3) wrong_direction
	if directionWrong && absDelta >= min {
		return Decision{Class: ClassWrongDirection, Score: 0, Evidence: marshal(ev)}
	}
	// 4) stale_no_move
	if absDelta < min && state == "stale" {
		return Decision{Class: ClassStaleNoMove, Score: 0.1, Evidence: marshal(ev)}
	}
	// 5) already_priced_noise
	if absDelta < min && repricing == "already_priced" {
		return Decision{Class: ClassAlreadyPricedNoise, Score: 0.15, Evidence: marshal(ev)}
	}
	// 6) correct_but_late
	if directionCorrect && repricing == "already_priced" {
		return Decision{Class: ClassCorrectButLate, Score: 0.4, Evidence: marshal(ev)}
	}
	// 7) useful_early
	if directionCorrect && isEarlyHorizon(in.Horizon, in.UsefulEarlyWindow) {
		return Decision{Class: ClassUsefulEarly, Score: 0.9, Evidence: marshal(ev)}
	}
	// 8) useful_correct
	if directionCorrect {
		return Decision{Class: ClassUsefulCorrect, Score: 0.75, Evidence: marshal(ev)}
	}
	// Fall-through: small move + neutral side or undetermined.
	return Decision{Class: ClassInsufficientData, Score: 0.1, Evidence: marshal(ev)}
}

// isEarlyHorizon reports whether `h` is at-or-shorter than the
// configured useful-early window. Default: "6h".
func isEarlyHorizon(h, earlyWindow string) bool {
	if earlyWindow == "" {
		earlyWindow = "6h"
	}
	order := map[string]int{"1h": 1, "6h": 6, "24h": 24, "72h": 72}
	return order[h] > 0 && order[h] <= order[earlyWindow]
}

func signOf(v float64) float64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

func marshal(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
