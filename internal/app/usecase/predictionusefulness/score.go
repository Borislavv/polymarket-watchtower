// Package predictionusefulness produces a deterministic operator-
// value score in [0,1] for every active prediction. The score is
// the worker-friendly rank we use for:
//
//   - dashboards (top-N panel);
//   - Telegram-send priority (high-priority threshold);
//   - future de-spam ("low usefulness → never ship");
//
// Everything in this package is pure deterministic math; the AI is
// NEVER involved. The contract is: given the same Inputs, the same
// Score comes out. Operators can audit the Components map to see
// why a score is high/low.
package predictionusefulness

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Inputs gather everything the scorer needs. The caller fills only
// the layers it has — missing fields penalise the score rather than
// crash. The struct mirrors what the evolution worker already
// collects per cycle so wiring is one line.
type Inputs struct {
	Prediction          repository.MarketPrediction
	Catalysts           []repository.EventCatalyst
	Repricing           *repricing.Signal
	Flow                eventflow.EventFlowSummary
	MatchedAlerts       int
	AnnotationCount     int
	HasRecentAnnotation bool
	// LifecycleEndDate populates from the prediction's market end
	// date. Used to score "urgency" — markets that resolve soon are
	// more useful than markets that resolve in a year.
	LifecycleEndDate time.Time
	// EventLastTradePrice is the price from the event-page market
	// snapshot, used to score price asymmetry (extremes near 0/1
	// are less actionable than mid-range odds).
	EventLastTradePrice float64
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
}

// Score is the canonical output. Score in [0,1]; Components map
// each summand to its contribution so the operator can audit. Reason
// is a stable human-readable string.
type Score struct {
	Score      float64
	Components map[string]float64
	Reason     string
}

// MarshalComponents serialises Components to JSON for the
// usefulness_scores.components_json column. Capped/safe — fails
// silently to empty bytes so the persist path never blocks on this.
func (s Score) MarshalComponents() []byte {
	if len(s.Components) == 0 {
		return nil
	}
	b, err := json.Marshal(s.Components)
	if err != nil {
		return nil
	}
	return b
}

// weights are the relative contributions of each component to the
// final score. They are deliberately small integers so an operator
// can sum the column visually. The sum below is 100 so each weight
// reads as "percent of max usefulness".
const (
	weightConfidence = 15.0
	weightCatalyst   = 15.0
	weightRepricing  = 20.0
	weightFlow       = 15.0
	weightAlertMatch = 10.0
	weightAnnotation = 10.0
	weightLifecycle  = 8.0
	weightAsymmetry  = 4.0
	weightFreshness  = 3.0
	totalWeight      = 100.0
)

// Compute returns a deterministic Score for one prediction. The
// scoring rules:
//
//   - confidence: pred.Confidence ∈ [0,1] times its weight.
//   - catalyst: 1 when ≥1 active/expected catalyst, 0 otherwise.
//   - repricing: explicit map per status — underreacting / reversed
//     are most actionable, already_priced / unclear penalised.
//   - flow: presence of recent same-side flow.
//   - alert_match: clamped to weight per matched alert.
//   - annotation: 1 when ≥3 fresh annotations OR a recent move.
//   - lifecycle: cosine-style urgency — closer to end_date scores
//     higher (markets resolving in <72h get full credit).
//   - asymmetry: max odds penalty when the market is at 95+% or
//     <5% — there's no edge left to capture.
//   - freshness: how recently the prediction was updated.
//
// "Bad signal" inputs (neutral, already_priced, stale, no flow, no
// catalyst, no annotations) collapse the score naturally because
// every component returns 0 in those cases. We don't subtract;
// every component is in [0, weight].
func Compute(in Inputs) Score {
	if in.Now == nil {
		in.Now = time.Now
	}
	now := in.Now()
	c := map[string]float64{}

	// Confidence — already 0..1.
	c["confidence"] = clamp01(in.Prediction.Confidence) * weightConfidence

	// Catalyst presence — 1 if any expected/active.
	catalystHit := 0.0
	for _, k := range in.Catalysts {
		s := strings.ToLower(string(k.Status))
		if s == "expected" || s == "active" {
			catalystHit = 1.0
			break
		}
	}
	c["catalyst"] = catalystHit * weightCatalyst

	// Repricing — explicit per-status map. Missing or unclear are 0.
	c["repricing"] = repricingComponent(in.Repricing) * weightRepricing

	// Flow — same-side recent flow > opposite-side ⇒ confirms.
	flowComp := 0.0
	if in.Flow.SameSideNotionalUSD > 0 {
		// Ratio of same-side to total recent notional, clamped.
		total := in.Flow.SameSideNotionalUSD + in.Flow.OppositeSideNotionalUSD
		if total > 0 {
			flowComp = clamp01(in.Flow.SameSideNotionalUSD / total)
		}
	}
	c["flow"] = flowComp * weightFlow

	// Alert match strength — log-ish; 0 alerts → 0, 5+ alerts → full.
	alertComp := math.Min(float64(in.MatchedAlerts)/5.0, 1.0)
	c["alert_match"] = alertComp * weightAlertMatch

	// Annotations — at least 3 recent OR HasRecentAnnotation.
	annComp := 0.0
	if in.HasRecentAnnotation {
		annComp = 1.0
	} else if in.AnnotationCount >= 3 {
		annComp = 0.6
	} else if in.AnnotationCount >= 1 {
		annComp = 0.3
	}
	c["annotation"] = annComp * weightAnnotation

	// Lifecycle urgency — closer to end_date = more useful.
	c["lifecycle"] = lifecycleUrgency(in.LifecycleEndDate, now) * weightLifecycle

	// Asymmetry — 0..1; mid-range odds score full, extremes 0.
	c["asymmetry"] = asymmetryScore(in.EventLastTradePrice) * weightAsymmetry

	// Freshness — penalty when prediction hasn't been touched in 24h+.
	c["freshness"] = freshnessScore(in.Prediction.UpdatedAt, now) * weightFreshness

	// State penalties — already_priced / stale / contradicted are
	// half-credit at most regardless of components.
	stateMul := stateMultiplier(in.Prediction.CurrentState)
	for k, v := range c {
		c[k] = v * stateMul
	}

	// Sum + normalise to 0..1.
	var sum float64
	for _, v := range c {
		sum += v
	}
	score := clamp01(sum / totalWeight)

	return Score{
		Score:      score,
		Components: c,
		Reason:     reasonFor(score, c, in.Prediction.CurrentState, stateMul),
	}
}

// repricingComponent maps a repricing status to a 0..1 weight.
// underreacting / reversed are the most actionable signals (price
// hasn't caught up / has overcorrected); already_priced kills the
// score because there's no edge left.
func repricingComponent(sig *repricing.Signal) float64 {
	if sig == nil {
		return 0
	}
	switch sig.RepricingStatus {
	case repricing.StatusUnderreacting:
		return 1.0
	case repricing.StatusReversed:
		return 0.85
	case repricing.StatusOverreacting:
		return 0.55
	case repricing.StatusStillRepricing:
		return 0.50
	case repricing.StatusLaggingRelatedOutcome:
		return 0.7
	case repricing.StatusAlreadyPriced, repricing.StatusStaleAnnotation:
		return 0
	}
	return 0
}

// lifecycleUrgency returns 1.0 when end_date is < 72h away, 0.6 for
// < 7d, 0.3 for < 30d, 0.1 for further; 0 when end_date is unknown
// or already past.
func lifecycleUrgency(end time.Time, now time.Time) float64 {
	if end.IsZero() {
		return 0.2 // unknown — slight residual usefulness
	}
	remaining := end.Sub(now)
	if remaining <= 0 {
		return 0
	}
	switch {
	case remaining < 72*time.Hour:
		return 1.0
	case remaining < 7*24*time.Hour:
		return 0.6
	case remaining < 30*24*time.Hour:
		return 0.3
	}
	return 0.1
}

// asymmetryScore penalises predictions on markets pinned to 0% or
// 100% — there's no edge left to capture. Mid-range (0.2–0.8) gets
// full credit; extremes (≥0.95 or ≤0.05) get zero.
func asymmetryScore(price float64) float64 {
	if price <= 0 || price >= 1 {
		return 0
	}
	// Distance from extremes, normalised. dist=0 at 0 or 1, dist=0.5
	// at 0.5. We map [0, 0.05] → 0, [0.05, 0.2] → 0..1, [0.2, 0.8]
	// → 1, [0.8, 0.95] → 1..0, [0.95, 1] → 0.
	dist := math.Min(price, 1-price)
	switch {
	case dist >= 0.2:
		return 1.0
	case dist >= 0.05:
		return (dist - 0.05) / (0.2 - 0.05)
	}
	return 0
}

// freshnessScore returns 1.0 when the prediction's last update is
// within the last hour, decaying linearly to 0 at 24h.
func freshnessScore(updated, now time.Time) float64 {
	if updated.IsZero() {
		return 0
	}
	age := now.Sub(updated)
	if age <= time.Hour {
		return 1.0
	}
	if age >= 24*time.Hour {
		return 0
	}
	return 1.0 - float64(age-time.Hour)/float64(23*time.Hour)
}

// stateMultiplier applies a hard penalty for terminal / undesirable
// states. The pure score is computed unchanged; this multiplier
// collapses the final value so the dashboard ranks living
// predictions above dead ones.
func stateMultiplier(state string) float64 {
	switch state {
	case "resolved", "invalidated":
		return 0
	case "stale", "already_priced":
		return 0.5
	case "contradicted_by_flow":
		return 0.6
	case "confirmed_by_flow", "active_catalyst":
		return 1.0
	case "blocked", "repricing", "watching", "new":
		return 0.9
	}
	return 0.7
}

// reasonFor packs the most-actionable components into a single
// short sentence the dashboard + Telegram render show. Stable
// wording so a future eyeballed review can be diff'd.
func reasonFor(score float64, c map[string]float64, state string, stateMul float64) string {
	parts := make([]string, 0, 4)
	if score >= 0.80 {
		parts = append(parts, "high-priority")
	} else if score >= 0.60 {
		parts = append(parts, "actionable")
	} else if score >= 0.40 {
		parts = append(parts, "borderline")
	} else {
		parts = append(parts, "low-value")
	}
	if c["catalyst"] > 0 {
		parts = append(parts, "catalyst present")
	}
	if c["repricing"] >= weightRepricing*0.8 {
		parts = append(parts, "repricing actionable")
	}
	if c["flow"] >= weightFlow*0.6 {
		parts = append(parts, "flow confirms")
	}
	if c["alert_match"] >= weightAlertMatch*0.6 {
		parts = append(parts, "alerts aligned")
	}
	if stateMul < 1.0 {
		parts = append(parts, "state="+state)
	}
	return strings.Join(parts, "; ")
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
