package strategypromotion

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeSamples struct {
	rows    []Sample
	buckets []BucketSample
	err     error
}

func (f *fakeSamples) ListPromotionSamples(_ context.Context, _ time.Duration) ([]Sample, error) {
	return f.rows, nil
}

func (f *fakeSamples) ListPromotionBucketSamples(_ context.Context, _ time.Duration) ([]BucketSample, error) {
	return f.buckets, f.err
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

// v11.10 PART 7 — bucketed promotion review tests.

func TestEvaluateBuckets_EmptyInputProducesEmptyDiagnostics(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	b := w.evaluateBuckets("thesisaccum", "v11.5", nil)
	if len(b.ByDecisionLevel) != 0 || len(b.ByLinkage) != 0 {
		t.Fatalf("empty bucket map must yield empty diagnostics, got %+v", b)
	}
}

func TestEvaluateBuckets_PerBucketEligibilityFollowsSameRules(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	idx := indexBucketSamples([]BucketSample{
		{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "decision_level", Key: "warning",
			SampleSize: 100, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 10},
		{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "decision_level", Key: "info",
			SampleSize: 5, MedianSignedMove6h: 1.0, Reversal15mRatio: 0.1, AlertsPerDay: 1},
		{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "linkage", Key: "linked",
			SampleSize: 100, MedianSignedMove6h: 2.0, Reversal15mRatio: 0.3, AlertsPerDay: 20},
		{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "linkage", Key: "standalone",
			SampleSize: 80, MedianSignedMove6h: 0.5, Reversal15mRatio: 0.1, AlertsPerDay: 5},
	})
	b := w.evaluateBuckets("thesisaccum", "v11.5", idx)
	if len(b.ByDecisionLevel) != 2 {
		t.Fatalf("expected 2 decision_level buckets, got %d", len(b.ByDecisionLevel))
	}
	// info: sample_size=5 < MinSampleSize=50 → ineligible
	if b.ByDecisionLevel[0].Key != "info" || b.ByDecisionLevel[0].Eligible {
		t.Fatalf("info bucket must be ineligible (small sample), got %+v", b.ByDecisionLevel[0])
	}
	// warning: passes all thresholds
	if b.ByDecisionLevel[1].Key != "warning" || !b.ByDecisionLevel[1].Eligible {
		t.Fatalf("warning bucket must be eligible, got %+v", b.ByDecisionLevel[1])
	}
	// linkage:linked passes; standalone fails on median floor
	if !b.ByLinkage[0].Eligible {
		t.Fatalf("linked bucket must be eligible, got %+v", b.ByLinkage[0])
	}
	if b.ByLinkage[1].Eligible {
		t.Fatalf("standalone bucket must be ineligible (median<floor), got %+v", b.ByLinkage[1])
	}
}

func TestTick_BucketVetoBlocksHealthyAggregate(t *testing.T) {
	// Whole-strategy aggregate is healthy, but every bucket is either
	// under-sampled or failing → overall must NOT be eligible.
	samples := &fakeSamples{
		rows: []Sample{{
			StrategyName: "thesisaccum", StrategyVersion: "v11.5",
			SampleSize: 200, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 15,
		}},
		buckets: []BucketSample{
			{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "decision_level", Key: "info",
				SampleSize: 10, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 1},
			{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "linkage", Key: "linked",
				SampleSize: 5, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 1},
		},
	}
	writer := &fakeWriter{}
	w := New(newCfg(), samples, writer, nil, nil)
	w.Tick(context.Background())
	if len(writer.reviews) != 1 {
		t.Fatalf("expected 1 review row; got %d", len(writer.reviews))
	}
	r := writer.reviews[0]
	if r.Eligible {
		t.Fatalf("bucket veto must force Eligible=false; got %+v", r)
	}
	if len(r.Reasons) == 0 || r.Reasons[0] != "no_eligible_non_trivial_bucket" {
		t.Fatalf("expected no_eligible_non_trivial_bucket reason, got %+v", r.Reasons)
	}
	if w.Allow("thesisaccum") {
		t.Fatalf("Allow must return false when bucket veto fires")
	}
}

func TestTick_BucketDiagnosticsPersisted(t *testing.T) {
	samples := &fakeSamples{
		rows: []Sample{{
			StrategyName: "thesisaccum", StrategyVersion: "v11.5",
			SampleSize: 200, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 15,
		}},
		buckets: []BucketSample{
			{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "decision_level", Key: "warning",
				SampleSize: 100, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 10},
			{StrategyName: "thesisaccum", StrategyVersion: "v11.5", Dimension: "linkage", Key: "linked",
				SampleSize: 100, MedianSignedMove6h: 2.5, Reversal15mRatio: 0.2, AlertsPerDay: 10},
		},
	}
	writer := &fakeWriter{}
	w := New(newCfg(), samples, writer, nil, nil)
	w.Tick(context.Background())
	if len(writer.reviews) != 1 {
		t.Fatalf("expected 1 review")
	}
	if len(writer.reviews[0].Buckets.ByDecisionLevel) == 0 {
		t.Fatalf("decision_level diagnostics must be persisted")
	}
	if len(writer.reviews[0].Buckets.ByLinkage) == 0 {
		t.Fatalf("linkage diagnostics must be persisted")
	}
	if !writer.reviews[0].Eligible {
		t.Fatalf("must remain eligible when an eligible non-trivial bucket exists, got %+v", writer.reviews[0])
	}
}
