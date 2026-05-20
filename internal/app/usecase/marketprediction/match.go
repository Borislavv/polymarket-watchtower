package marketprediction

import (
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// MatchedAlert is the deterministic pre-score the state machine
// consumes. AI may refine top candidates later; for now the score
// is a transparent linear combination of identity + direction +
// timing signals.
type MatchedAlert struct {
	AlertID            int64
	Severity           string
	Kind               string
	ConditionID        string
	EventSlug          string
	Outcome            string
	Side               string
	Score              float64
	DirectionAlignment string // "aligned" | "contradict" | "neutral"
	MatchReason        string
	MatchedOn          []string
	AlertAt            time.Time
}

// AlertCandidate is the input — typically built from anomaly.Finding
// at alertsender time or replayed from polymarket_alerts.
type AlertCandidate struct {
	AlertID          int64
	Severity         anomaly.Severity
	Kind             anomaly.Kind
	ConditionID      string
	EventSlug        string
	Outcome          string
	Side             string
	AnnotationHashes []string // optional — when the alert references annotation events
	At               time.Time
}

// PredictionRef is the relevant slice of a prediction for matching.
type PredictionRef struct {
	EventSlug   string
	ConditionID string
	Outcome     string
	SideBias    string // operator-set: BUY / SELL / "" (no bias)
	CreatedAt   time.Time
}

// Score computes a deterministic match score in [0, 1] plus the
// direction alignment. The score weights identity (event + market
// + outcome), direction alignment, and severity. Empty score
// reasons disclose every contribution so the operator can audit.
//
// Heuristic weights (must sum to ≤ 1):
//   - same event_slug:       0.25
//   - same condition_id:     0.20
//   - same outcome:          0.15
//   - direction alignment:   0.20 (signed; neutral=0)
//   - severity floor:        0.10 (warning+ adds, info doesn't)
//   - timing (≤6h before prediction): 0.10
func Score(alert AlertCandidate, pred PredictionRef) MatchedAlert {
	matched := MatchedAlert{
		AlertID:     alert.AlertID,
		Severity:    string(alert.Severity),
		Kind:        string(alert.Kind),
		ConditionID: alert.ConditionID,
		EventSlug:   alert.EventSlug,
		Outcome:     alert.Outcome,
		Side:        alert.Side,
		AlertAt:     alert.At,
	}
	var (
		score        float64
		matchedOn    []string
		alignmentVal int // -1 contradict / 0 neutral / +1 aligned
	)

	if pred.EventSlug != "" && strings.EqualFold(pred.EventSlug, alert.EventSlug) {
		score += 0.25
		matchedOn = append(matchedOn, "event_slug")
	}
	if pred.ConditionID != "" && strings.EqualFold(pred.ConditionID, alert.ConditionID) {
		score += 0.20
		matchedOn = append(matchedOn, "condition_id")
	}
	if pred.Outcome != "" && strings.EqualFold(pred.Outcome, alert.Outcome) {
		score += 0.15
		matchedOn = append(matchedOn, "outcome")
	}

	// Direction alignment.
	if pred.SideBias != "" && alert.Side != "" {
		switch {
		case strings.EqualFold(pred.SideBias, alert.Side):
			score += 0.20
			alignmentVal = +1
			matchedOn = append(matchedOn, "side_aligned")
		case oppositeSide(pred.SideBias, alert.Side):
			score -= 0.20
			alignmentVal = -1
			matchedOn = append(matchedOn, "side_opposed")
		default:
			alignmentVal = 0
		}
	}

	// Severity floor — warning+ adds confidence.
	switch alert.Severity {
	case anomaly.SeverityWarning:
		score += 0.05
		matchedOn = append(matchedOn, "severity_warning")
	case anomaly.SeverityCritical:
		score += 0.08
		matchedOn = append(matchedOn, "severity_critical")
	case anomaly.SeverityHard:
		score += 0.10
		matchedOn = append(matchedOn, "severity_hard")
	}

	// Timing — alerts within 6h before the prediction update get a
	// bump (the alert helped form the thesis); alerts AFTER the
	// prediction are "evidence" but only count when within 24h.
	if !pred.CreatedAt.IsZero() && !alert.At.IsZero() {
		delta := alert.At.Sub(pred.CreatedAt)
		switch {
		case delta < 0 && -delta <= 6*time.Hour:
			score += 0.10
			matchedOn = append(matchedOn, "timing_pre_pred_6h")
		case delta >= 0 && delta <= 24*time.Hour:
			score += 0.06
			matchedOn = append(matchedOn, "timing_post_pred_24h")
		}
	}

	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	matched.Score = score
	matched.MatchedOn = matchedOn
	switch {
	case alignmentVal > 0:
		matched.DirectionAlignment = "aligned"
	case alignmentVal < 0:
		matched.DirectionAlignment = "contradict"
	default:
		matched.DirectionAlignment = "neutral"
	}
	matched.MatchReason = strings.Join(matchedOn, ",")
	return matched
}

func oppositeSide(a, b string) bool {
	a = strings.ToUpper(strings.TrimSpace(a))
	b = strings.ToUpper(strings.TrimSpace(b))
	return (a == "BUY" && b == "SELL") || (a == "SELL" && b == "BUY")
}
