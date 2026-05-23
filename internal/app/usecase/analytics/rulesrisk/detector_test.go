package rulesrisk

import "testing"

// Title with runoff + certified + court + appeal + recount + first
// → ≥ 0.7 → block_both.
func TestDecide_HighRiskBlocksBoth(t *testing.T) {
	in := Input{
		Title:           "Will candidate X be certified after runoff, recount, and first court ruling",
		ResolutionRules: "Resolves YES if appeal is denied",
	}
	v := New(Config{}).Decide(in)
	if v.AmbiguityScore < 0.7 {
		t.Fatalf("expected high ambiguity ≥ 0.7; got %v", v.AmbiguityScore)
	}
	if v.Action != ActionBlockBoth {
		t.Errorf("expected block_both; got %s", v.Action)
	}
}

// Simple binary with explicit source → low risk → allow.
func TestDecide_SimpleBinaryAllows(t *testing.T) {
	in := Input{
		Title:           "Will the S&P 500 close above 6000 on 2026-12-31",
		ResolutionRules: "Resolves YES based on Yahoo Finance EOD price",
	}
	v := New(Config{}).Decide(in)
	if v.AmbiguityScore >= 0.6 {
		t.Errorf("simple market must score low; got %v", v.AmbiguityScore)
	}
	if v.Action != ActionAllow {
		t.Errorf("expected allow; got %s", v.Action)
	}
}

// Runoff + court + appeal + announced + official → score above the
// HighRiskThreshold (cap_severity floor) but below BlockBoth.
func TestDecide_MidRiskHitsActionGate(t *testing.T) {
	in := Input{
		Title:           "Will X be officially announced after runoff and court appeal",
		ResolutionRules: "",
	}
	v := New(Config{}).Decide(in)
	if v.AmbiguityScore < 0.3 {
		t.Fatalf("expected ambiguity ≥ 0.3; got %v", v.AmbiguityScore)
	}
	if v.Action == ActionAllow {
		t.Fatalf("must NOT allow with runoff+court+appeal+announced+official; got %s (score=%v)",
			v.Action, v.AmbiguityScore)
	}
}
