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

// TestDecide_ProceduralComplexityBonus pins the v11.10 stricter
// scoring: three procedural markers (court + appeal + certification)
// trigger an explicit complexity bonus on top of per-marker weights.
func TestDecide_ProceduralComplexityBonus(t *testing.T) {
	in := Input{
		Title:           "X court appeal certification",
		Description:     "",
		ResolutionRules: "",
	}
	v := New(Config{}).Decide(in)
	procedural := 0
	for _, r := range v.Reasons {
		if r == "procedural_complexity_high" || r == "procedural_complexity_medium" {
			procedural = 1
		}
	}
	if procedural != 1 {
		t.Fatalf("expected procedural_complexity_{high,medium} in reasons; got %v", v.Reasons)
	}
	if hits, ok := v.Features["procedural_hits"].(int); !ok || hits < 3 {
		t.Fatalf("procedural_hits must be ≥ 3; got %v", v.Features["procedural_hits"])
	}
}

// TestDecide_WordingRiskBonus pins the wording-risk dimension. Three
// subjective qualifiers should bump the score noticeably and emit
// wording_risk_high.
func TestDecide_WordingRiskBonus(t *testing.T) {
	in := Input{
		Title:           "Will X be considered significant and material in primarily this region",
		ResolutionRules: "",
	}
	v := New(Config{}).Decide(in)
	hasWordingHigh := false
	for _, r := range v.Reasons {
		if r == "wording_risk_high" {
			hasWordingHigh = true
		}
	}
	if !hasWordingHigh {
		t.Fatalf("expected wording_risk_high; got %v", v.Reasons)
	}
}

// TestDecide_NamedSourceOffset proves naming a trusted source
// partially offsets the "official" / "announced" dependency penalty.
func TestDecide_NamedSourceOffset(t *testing.T) {
	unnamed := New(Config{}).Decide(Input{
		Title:           "Will X be officially announced",
		ResolutionRules: "Resolves on official announcement",
	})
	named := New(Config{}).Decide(Input{
		Title:           "Will X be officially announced",
		ResolutionRules: "Resolves on official announcement, according to Reuters reporting",
	})
	if named.AmbiguityScore >= unnamed.AmbiguityScore {
		t.Fatalf("named source must lower ambiguity vs unnamed (named=%v unnamed=%v)",
			named.AmbiguityScore, unnamed.AmbiguityScore)
	}
}

// TestDecide_BlockRepricingActionAtHighScore proves the block
// action is actually triggered at the configured threshold.
func TestDecide_BlockRepricingActionAtHighScore(t *testing.T) {
	in := Input{
		Title:           "Will candidate X be certified after recount, runoff, court appeal, and injunction ruling",
		ResolutionRules: "Resolves YES if officially announced",
	}
	v := New(Config{HighRiskThreshold: 0.50, BlockRepricingAt: 0.50, BlockCheaptailAt: 0.50}).Decide(in)
	if v.Action != ActionBlockBoth {
		t.Fatalf("expected block_both at high score (score=%v); got %s", v.AmbiguityScore, v.Action)
	}
}
