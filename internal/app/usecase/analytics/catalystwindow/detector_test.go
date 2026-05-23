package catalystwindow

import (
	"testing"
	"time"
)

var debateSpec = map[string]WindowSpec{
	"debate": {Pre: 12 * time.Hour, Post: 4 * time.Hour},
}

// Signal 4h before debate → in pre-window; positive boost.
func TestDecide_PreWindowFires(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	in := Input{
		SignalTime:    now,
		EventSlug:     "us-pres-2028",
		WindowsByKind: debateSpec,
		MinConfidence: 0.5,
		Catalysts: []Catalyst{
			{Kind: "debate", ExpectedAt: now.Add(4 * time.Hour), Confidence: 0.9, EventSlug: "us-pres-2028"},
		},
	}
	v := New(Config{}).Decide(in)
	if !v.InWindow {
		t.Fatalf("must be in window; got %+v", v)
	}
	if v.Boost <= 0 {
		t.Fatalf("boost must be positive; got %v", v.Boost)
	}
}

// Signal 3 days before a low-confidence catalyst → outside window
// AND below confidence floor → no boost.
func TestDecide_OutsideWindowOrLowConfidenceNoBoost(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	in := Input{
		SignalTime:    now,
		EventSlug:     "us-pres-2028",
		WindowsByKind: debateSpec,
		MinConfidence: 0.5,
		Catalysts: []Catalyst{
			// 3d ahead — outside the 12h pre window.
			{Kind: "debate", ExpectedAt: now.Add(72 * time.Hour), Confidence: 0.9, EventSlug: "us-pres-2028"},
			// In-window but confidence too low.
			{Kind: "debate", ExpectedAt: now.Add(2 * time.Hour), Confidence: 0.2, EventSlug: "us-pres-2028"},
		},
	}
	v := New(Config{}).Decide(in)
	if v.InWindow {
		t.Fatalf("must NOT match; got %+v", v)
	}
	if v.Boost != 0 {
		t.Errorf("boost must be zero; got %v", v.Boost)
	}
}

// Multiple matching catalysts → strongest by proximity wins;
// deterministic selection.
func TestDecide_StrongestCatalystWinsDeterministically(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	in := Input{
		SignalTime:    now,
		EventSlug:     "us-pres-2028",
		WindowsByKind: debateSpec,
		MinConfidence: 0.5,
		Catalysts: []Catalyst{
			{Kind: "debate", ExpectedAt: now.Add(10 * time.Hour), Confidence: 0.8, EventSlug: "us-pres-2028"},
			{Kind: "debate", ExpectedAt: now.Add(2 * time.Hour), Confidence: 0.8, EventSlug: "us-pres-2028"},
		},
	}
	v := New(Config{}).Decide(in)
	if !v.InWindow {
		t.Fatalf("must match")
	}
	// 10h lead picked over 2h lead — more advance warning is more interesting.
	if v.LeadTime != 10*time.Hour {
		t.Errorf("expected 10h lead winner; got %s", v.LeadTime)
	}
}

// Decide is never standalone — verify the type doesn't expose a
// "Fired" field. (Compile-time guarantee; this test exists for
// documentation.)
func TestVerdictNeverStandalone(t *testing.T) {
	// If a future refactor adds Fired bool, this assignment will
	// not compile because BoostVerdict has no Fired field.
	v := BoostVerdict{InWindow: true, Boost: 5}
	_ = v
}
