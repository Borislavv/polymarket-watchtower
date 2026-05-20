package predictionevaluation

import "testing"

func fp(v float64) *float64 { return &v }

// TestClassify_UsefulCorrect pins the canonical correct-direction +
// not-already-priced case at 24h.
func TestClassify_UsefulCorrect(t *testing.T) {
	d := Classify(Inputs{
		Horizon:                  "24h",
		SideBias:                 "bullish",
		PriceAtPrediction:        fp(0.40),
		PriceAtHorizon:           fp(0.55), // +15%
		StateAtHorizon:           "watching",
		RepricingStatusAtHorizon: "underreacting",
	})
	if d.Class != ClassUsefulCorrect {
		t.Errorf("class: got %q want useful_correct", d.Class)
	}
}

// TestClassify_UsefulEarly pins the same direction at ≤ UsefulEarlyWindow.
func TestClassify_UsefulEarly(t *testing.T) {
	d := Classify(Inputs{
		Horizon:           "1h",
		UsefulEarlyWindow: "6h",
		SideBias:          "bullish",
		PriceAtPrediction: fp(0.40),
		PriceAtHorizon:    fp(0.55),
		StateAtHorizon:    "watching",
	})
	if d.Class != ClassUsefulEarly {
		t.Errorf("class: got %q want useful_early", d.Class)
	}
}

// TestClassify_WrongDirection pins the bearish + price-up = wrong case.
func TestClassify_WrongDirection(t *testing.T) {
	d := Classify(Inputs{
		Horizon:           "24h",
		SideBias:          "bearish",
		PriceAtPrediction: fp(0.40),
		PriceAtHorizon:    fp(0.55), // +15% but we predicted DOWN
	})
	if d.Class != ClassWrongDirection {
		t.Errorf("class: got %q want wrong_direction", d.Class)
	}
}

// TestClassify_StaleNoMove pins the stale-with-tiny-move bucket.
func TestClassify_StaleNoMove(t *testing.T) {
	d := Classify(Inputs{
		Horizon:           "24h",
		SideBias:          "bullish",
		PriceAtPrediction: fp(0.50),
		PriceAtHorizon:    fp(0.502), // 0.2% delta < 3% min
		StateAtHorizon:    "stale",
	})
	if d.Class != ClassStaleNoMove {
		t.Errorf("class: got %q want stale_no_move", d.Class)
	}
}

// TestClassify_AlreadyPricedNoise pins the small-delta +
// repricing=already_priced bucket.
func TestClassify_AlreadyPricedNoise(t *testing.T) {
	d := Classify(Inputs{
		Horizon:                  "24h",
		SideBias:                 "bullish",
		PriceAtPrediction:        fp(0.50),
		PriceAtHorizon:           fp(0.51),
		StateAtHorizon:           "already_priced",
		RepricingStatusAtHorizon: "already_priced",
	})
	if d.Class != ClassAlreadyPricedNoise {
		t.Errorf("class: got %q want already_priced_noise", d.Class)
	}
}

// TestClassify_CorrectButLate pins the right-direction +
// already_priced case (market moved before our horizon).
func TestClassify_CorrectButLate(t *testing.T) {
	d := Classify(Inputs{
		Horizon:                  "24h",
		SideBias:                 "bullish",
		PriceAtPrediction:        fp(0.40),
		PriceAtHorizon:           fp(0.55),
		RepricingStatusAtHorizon: "already_priced",
	})
	if d.Class != ClassCorrectButLate {
		t.Errorf("class: got %q want correct_but_late", d.Class)
	}
}

// TestClassify_BlockedUnresolved pins the blocked-without-catalyst case.
func TestClassify_BlockedUnresolved(t *testing.T) {
	d := Classify(Inputs{
		Horizon:           "24h",
		SideBias:          "bullish",
		PriceAtPrediction: fp(0.50),
		PriceAtHorizon:    fp(0.51),
		StateAtHorizon:    "blocked",
		CatalystResolved:  false,
	})
	if d.Class != ClassBlockedUnresolved {
		t.Errorf("class: got %q want blocked_unresolved", d.Class)
	}
}

// TestClassify_InsufficientData pins the missing-price case.
func TestClassify_InsufficientData(t *testing.T) {
	d := Classify(Inputs{
		Horizon:  "24h",
		SideBias: "bullish",
		// PriceAtPrediction left nil
		PriceAtHorizon: fp(0.55),
	})
	if d.Class != ClassInsufficientData {
		t.Errorf("class: got %q want insufficient_data", d.Class)
	}
}
