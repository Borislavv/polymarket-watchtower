package predictionusefulness

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestCompute_HighScoreWithCatalystAndFlowAndUnderreaction pins the
// canonical "this is a great signal" case: high confidence,
// active catalyst, repricing=underreacting, same-side flow > 80%.
func TestCompute_HighScoreWithCatalystAndFlowAndUnderreaction(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	in := Inputs{
		Prediction: repository.MarketPrediction{
			ID: 1, Confidence: 0.85, CurrentState: "watching", UpdatedAt: now.Add(-30 * time.Minute),
		},
		Catalysts: []repository.EventCatalyst{{Status: "expected"}},
		Repricing: &repricing.Signal{RepricingStatus: repricing.StatusUnderreacting},
		Flow: eventflow.EventFlowSummary{
			SameSideNotionalUSD:     900_000,
			OppositeSideNotionalUSD: 100_000,
		},
		MatchedAlerts:       6,
		HasRecentAnnotation: true,
		LifecycleEndDate:    now.Add(60 * time.Hour), // <72h ⇒ urgency=1
		EventLastTradePrice: 0.55,
		Now:                 fixedNow(now),
	}
	s := Compute(in)
	if s.Score < 0.80 {
		t.Errorf("expected high score (≥0.80); got %.2f, components=%v", s.Score, s.Components)
	}
}

// TestCompute_LowScoreAlreadyPriced pins the "edge gone" case.
// already_priced repricing → repricing component = 0; state stays
// "already_priced" (multiplier 0.5) so the score collapses.
func TestCompute_LowScoreAlreadyPriced(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Prediction: repository.MarketPrediction{
			Confidence: 0.7, CurrentState: "already_priced", UpdatedAt: now,
		},
		Repricing: &repricing.Signal{RepricingStatus: repricing.StatusAlreadyPriced},
		Now:       fixedNow(now),
	}
	s := Compute(in)
	if s.Score > 0.30 {
		t.Errorf("expected low score; got %.2f", s.Score)
	}
}

// TestCompute_LowScoreStaleNoSignal pins the "nothing happening"
// case: stale state, no catalyst, no flow, no annotations, no
// alerts.
func TestCompute_LowScoreStaleNoSignal(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Prediction: repository.MarketPrediction{
			Confidence: 0.3, CurrentState: "stale", UpdatedAt: now.Add(-72 * time.Hour),
		},
		Now: fixedNow(now),
	}
	s := Compute(in)
	if s.Score > 0.15 {
		t.Errorf("expected very low score; got %.2f", s.Score)
	}
	if s.Reason == "" {
		t.Error("missing reason")
	}
}

// TestCompute_TerminalStateZero pins the resolved/invalidated cap:
// no matter what other inputs say, terminal predictions score 0.
func TestCompute_TerminalStateZero(t *testing.T) {
	in := Inputs{
		Prediction: repository.MarketPrediction{
			Confidence: 1.0, CurrentState: "resolved",
		},
		Catalysts: []repository.EventCatalyst{{Status: "active"}},
		Repricing: &repricing.Signal{RepricingStatus: repricing.StatusUnderreacting},
	}
	s := Compute(in)
	if s.Score != 0 {
		t.Errorf("resolved must be 0; got %.2f", s.Score)
	}
}

// TestCompute_ComponentsMapIsAuditable pins the contract that the
// dashboard panel can read components_json and reconstruct the
// score. Every named component must appear (zero values OK).
func TestCompute_ComponentsMapIsAuditable(t *testing.T) {
	in := Inputs{Prediction: repository.MarketPrediction{Confidence: 0.5, CurrentState: "watching"}}
	s := Compute(in)
	for _, want := range []string{
		"confidence", "catalyst", "repricing", "flow",
		"alert_match", "annotation", "lifecycle", "asymmetry", "freshness",
	} {
		if _, ok := s.Components[want]; !ok {
			t.Errorf("components map missing %q: %v", want, s.Components)
		}
	}
}

// TestAsymmetryScore pins the price-extreme handling so the
// dashboard panel doesn't suddenly reward 99% predictions.
func TestAsymmetryScore(t *testing.T) {
	cases := []struct{ price, want float64 }{
		{0.50, 1.0},
		{0.30, 1.0},
		{0.20, 1.0},
		{0.12, 0.466},
		{0.95, 0.0},
		{0.99, 0.0},
		{0.01, 0.0},
	}
	for _, tc := range cases {
		got := asymmetryScore(tc.price)
		if absDiff(got, tc.want) > 0.01 {
			t.Errorf("asymmetryScore(%.2f): got %.3f want %.3f", tc.price, got, tc.want)
		}
	}
}

func absDiff(a, b float64) float64 {
	if a < b {
		return b - a
	}
	return a - b
}
