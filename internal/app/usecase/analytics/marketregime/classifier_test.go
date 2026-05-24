package marketregime

import "testing"

func TestClassify_Geopolitics(t *testing.T) {
	v := New().Classify(Input{
		CategoryLabel: "Geopolitics",
		Title:         "Will Russia and Ukraine sign a ceasefire by 2026-12-31",
	})
	if v.Regime != RegimeGeopoliticsMilitary {
		t.Fatalf("expected geopolitics_military; got %s (reasons=%v)", v.Regime, v.Reasons)
	}
}

func TestClassify_PoliticsGovernance(t *testing.T) {
	v := New().Classify(Input{
		CategorySlug: "politics",
		Title:        "Will the Senate confirm the new Supreme Court nominee before recess",
	})
	if v.Regime != RegimePoliticsGovernance {
		t.Fatalf("expected politics_governance; got %s (reasons=%v)", v.Regime, v.Reasons)
	}
}

func TestClassify_CorporatePrivateInfo(t *testing.T) {
	v := New().Classify(Input{
		Title: "Will NVIDIA report Q3 revenue above $30B in next earnings call",
	})
	if v.Regime != RegimeCorporatePrivateInfo {
		t.Fatalf("expected corporate_private_info; got %s (reasons=%v)", v.Regime, v.Reasons)
	}
}

// Oracle keywords win even when geopolitics keywords are also
// present — oracle_sensitive has priority because user flow is
// blocked without dual confirmation on this regime (safety wins).
func TestClassify_OracleSensitivePrioritized(t *testing.T) {
	v := New().Classify(Input{
		Title:           "Will UMA dispute result resolve YES on the Ukraine ceasefire market",
		ResolutionRules: "Resolves YES per UMA oracle attester verifiable evidence",
	})
	if v.Regime != RegimeOracleSensitive {
		t.Fatalf("oracle keywords must win priority tiebreak; got %s (reasons=%v)", v.Regime, v.Reasons)
	}
	if !v.Regime.RequiresDualConfirmation() {
		t.Fatalf("oracle_sensitive must require dual confirmation")
	}
}

// Catch-all market with no strong keywords lands in 'other'.
func TestClassify_OtherCatchall(t *testing.T) {
	v := New().Classify(Input{
		Title: "Will the temperature in NYC exceed 90F on a random day in summer",
	})
	if v.Regime != RegimeOther {
		t.Fatalf("expected other; got %s (reasons=%v)", v.Regime, v.Reasons)
	}
	if v.Regime.RequiresDualConfirmation() {
		t.Fatalf("'other' regime must not require dual confirmation")
	}
}

// RequiresDualConfirmation gating is per-regime explicit.
func TestRequiresDualConfirmation(t *testing.T) {
	cases := map[Regime]bool{
		RegimeOracleSensitive:      true,
		RegimeGeopoliticsMilitary:  false,
		RegimePoliticsGovernance:   false,
		RegimeCorporatePrivateInfo: false,
		RegimeOther:                false,
	}
	for r, want := range cases {
		if got := r.RequiresDualConfirmation(); got != want {
			t.Errorf("%s: RequiresDualConfirmation=%v want %v", r, got, want)
		}
	}
}
