package strategypromotion

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeSamples struct {
	rows []Sample
}

func (f *fakeSamples) ListPromotionSamples(_ context.Context, _ time.Duration) ([]Sample, error) {
	return f.rows, nil
}

type fakeWriter struct {
	mu      sync.Mutex
	reviews []Review
}

func (w *fakeWriter) WritePromotionReview(_ context.Context, r Review) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reviews = append(w.reviews, r)
	return nil
}

func newCfg() Config {
	return Config{
		Enabled:              true,
		Interval:             time.Hour,
		Lookback:             14 * 24 * time.Hour,
		MinSampleSize:        50,
		MinSignedMove6hCents: 1.5,
		MaxReversal15mRatio:  0.5,
		MaxAlertsPerDay:      40,
	}
}

func TestEvaluate_NotEligibleWithSmallSample(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	r := w.evaluate(Sample{StrategyName: "thesisaccum", SampleSize: 20, MedianSignedMove6h: 2.0}, time.Now())
	if r.Eligible {
		t.Fatalf("must be ineligible at N<50: %+v", r)
	}
}

func TestEvaluate_EligibleWhenAllCriteriaPass(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	r := w.evaluate(Sample{
		StrategyName:       "thesisaccum",
		SampleSize:         120,
		MedianSignedMove6h: 2.3,
		Reversal15mRatio:   0.2,
		AlertsPerDay:       20,
	}, time.Now())
	if !r.Eligible {
		t.Fatalf("expected eligible; got %+v", r)
	}
}

func TestEvaluate_NotEligibleWhenReversalTooHigh(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	r := w.evaluate(Sample{
		SampleSize: 100, MedianSignedMove6h: 2.0,
		Reversal15mRatio: 0.6, AlertsPerDay: 10,
	}, time.Now())
	if r.Eligible {
		t.Fatalf("must be ineligible at reversal>ceiling")
	}
}

func TestAllow_NoEligibleEverReturnsFalse(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	if w.Allow("thesisaccum") {
		t.Fatalf("Allow must default false before any tick")
	}
}

func TestAllow_BypassExplicitForcesFalse(t *testing.T) {
	cfg := newCfg()
	cfg.BypassExplicit = true
	w := New(cfg, nil, nil, nil, nil)
	// Even if we cheat and set state directly the gate still says no.
	w.state["thesisaccum"] = true
	if w.Allow("thesisaccum") {
		t.Fatalf("BypassExplicit=true must force Allow=false")
	}
}

func TestTick_PersistsReviewsAndUpdatesGate(t *testing.T) {
	samples := &fakeSamples{rows: []Sample{
		{StrategyName: "thesisaccum", StrategyVersion: "v11.5", SampleSize: 100, MedianSignedMove6h: 2.0, Reversal15mRatio: 0.2, AlertsPerDay: 20},
		{StrategyName: "cheaptail", StrategyVersion: "v11.5", SampleSize: 10, MedianSignedMove6h: 5.0},
	}}
	writer := &fakeWriter{}
	w := New(newCfg(), samples, writer, nil, nil)
	w.Tick(context.Background())

	if len(writer.reviews) != 2 {
		t.Fatalf("expected 2 reviews; got %d", len(writer.reviews))
	}
	if !w.Allow("thesisaccum") {
		t.Fatalf("thesisaccum should be eligible after tick")
	}
	if w.Allow("cheaptail") {
		t.Fatalf("cheaptail should not be eligible (N=10)")
	}
}

func TestTick_NoOpWhenDepsMissing(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	w.Tick(context.Background()) // must not panic
}
