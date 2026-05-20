package aipreflight

import (
	"strings"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/aibudget"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TestPreflight_AllowsUnderCap pins the happy path: prompt fits the
// chars cap + bucket has headroom → Allow=true.
func TestPreflight_AllowsUnderCap(t *testing.T) {
	met := metrics.New()
	budget := aibudget.New(aibudget.Config{
		GlobalDailyBudgetUSD:  10,
		BucketDailyBudgetsUSD: map[string]float64{aibudget.BucketPredictionCreate: 5},
	}, met)
	p := New(SurfaceConfig{
		Surface:         "prediction_creation",
		BudgetBucket:    aibudget.BucketPredictionCreate,
		MaxInputChars:   100,
		MaxOutputTokens: 1200,
		EstCostUSD:      0.05,
	}, budget, met)
	dec := p.Run("small prompt", nil)
	if !dec.Allow {
		t.Fatalf("expected Allow=true; got %+v", dec)
	}
	if dec.MaxOutputTokens != 1200 {
		t.Errorf("MaxOutputTokens should propagate; got %d", dec.MaxOutputTokens)
	}
	if dec.EstCostUSD != 0.05 {
		t.Errorf("EstCostUSD should propagate; got %v", dec.EstCostUSD)
	}
}

// TestPreflight_CompactsOverCap pins the compaction path: prompt is
// over MaxInputChars, the compactor shrinks it under the cap, and
// the call is allowed.
func TestPreflight_CompactsOverCap(t *testing.T) {
	p := New(SurfaceConfig{
		Surface:       "test",
		MaxInputChars: 100,
		EstCostUSD:    0.01,
	}, nil, nil)
	long := strings.Repeat("x", 300)
	dec := p.Run(long, SimpleCompactor{Cap: 100})
	if !dec.Allow {
		t.Fatalf("expected Allow=true after compaction; got %+v", dec)
	}
	if len(dec.Prompt) > 100 {
		t.Errorf("compaction failed to enforce cap; got %d chars", len(dec.Prompt))
	}
	if !strings.Contains(dec.Prompt, "truncated") {
		t.Error("compactor should append a truncation marker")
	}
}

// TestPreflight_SkipsWhenCompactionInsufficient pins the
// "compaction couldn't get under the cap" path: Allow=false +
// Reason=chars_cap; no HTTP call should be issued by the caller.
func TestPreflight_SkipsWhenCompactionInsufficient(t *testing.T) {
	p := New(SurfaceConfig{
		Surface:       "test",
		MaxInputChars: 50,
	}, nil, nil)
	// SimpleCompactor with Cap=200 leaves the prompt > 50.
	dec := p.Run(strings.Repeat("x", 300), SimpleCompactor{Cap: 200})
	if dec.Allow {
		t.Fatal("expected Allow=false when compaction insufficient")
	}
	if dec.Reason != "chars_cap" {
		t.Errorf("reason: got %q want chars_cap", dec.Reason)
	}
}

// TestPreflight_BudgetDenialBubbles pins the budget gate: when
// aibudget.Allow returns false, the preflight surfaces the right
// reason code.
func TestPreflight_BudgetDenialBubbles(t *testing.T) {
	met := metrics.New()
	budget := aibudget.New(aibudget.Config{
		GlobalDailyBudgetUSD:  1.0,
		BucketDailyBudgetsUSD: map[string]float64{aibudget.BucketPredictionCreate: 0.01},
	}, met)
	p := New(SurfaceConfig{
		Surface:      "prediction_creation",
		BudgetBucket: aibudget.BucketPredictionCreate,
		EstCostUSD:   0.05,
	}, budget, met)
	dec := p.Run("small", nil)
	if dec.Allow {
		t.Fatal("expected Allow=false on bucket denial")
	}
	if dec.Reason != "budget_bucket_exhausted" {
		t.Errorf("reason: got %q want budget_bucket_exhausted", dec.Reason)
	}
}

// TestPreflight_NilBudgetFailsOpen pins the fail-open contract: a
// nil budget manager doesn't break the call path.
func TestPreflight_NilBudgetFailsOpen(t *testing.T) {
	p := New(SurfaceConfig{
		Surface:    "test",
		EstCostUSD: 0.05,
	}, nil, nil)
	dec := p.Run("x", nil)
	if !dec.Allow {
		t.Errorf("nil budget must fail-open; got %+v", dec)
	}
}

// TestSimpleCompactor_UnderCapPassThrough pins the cheap-path
// invariant: SimpleCompactor never modifies an under-cap prompt.
func TestSimpleCompactor_UnderCapPassThrough(t *testing.T) {
	c := SimpleCompactor{Cap: 100}
	got, dropped := c.Compact("short")
	if got != "short" || dropped != "" {
		t.Errorf("under-cap should pass through unchanged; got %q dropped=%q", got, dropped)
	}
}
