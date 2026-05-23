package conflictresolve

import "testing"

func TestDecide_StrongDominanceSuppressesLoser(t *testing.T) {
	in := ConflictInput{
		A: SideSignal{Side: "YES", WalletQualityScore: 0.9, HolderStrength: 0.8, ThesisBreadth: 0.7, CatalystProximity: 0.6, BookSupport: 0.4},
		B: SideSignal{Side: "NO", WalletQualityScore: 0.1, HolderStrength: 0.1},
	}
	v := New(Config{}).Decide(in)
	if v.WinningSide != "YES" {
		t.Fatalf("YES must win on this composite; got %+v", v)
	}
	if v.Action != ActionBoostWinnerSuppress {
		t.Errorf("expected suppress action; got %s", v.Action)
	}
}

func TestDecide_NearTieTagsUnresolved(t *testing.T) {
	in := ConflictInput{
		A: SideSignal{Side: "YES", WalletQualityScore: 0.5, HolderStrength: 0.4},
		B: SideSignal{Side: "NO", WalletQualityScore: 0.45, HolderStrength: 0.45},
	}
	v := New(Config{}).Decide(in)
	if v.Action != ActionTagUnresolved {
		t.Fatalf("near-tie must tag unresolved; got %+v", v)
	}
	if v.WinningSide != "" {
		t.Errorf("winning side must be empty for unresolved; got %s", v.WinningSide)
	}
}

// MM-like penalty drops A enough that B's lead crosses the
// MinDominance gate and B is named the winner.
func TestDecide_MMLikeSidePenalised(t *testing.T) {
	in := ConflictInput{
		// Without MMLike A would total 0.9 + 0.5 = 1.4; MMLike
		// penalty (0.4) drops it to 0.84.
		A: SideSignal{Side: "YES", WalletQualityScore: 0.9, HolderStrength: 0.5, MMLike: true},
		// B totals 0.9 + 0.7 + 0.5 = 2.1.
		B: SideSignal{Side: "NO", WalletQualityScore: 0.9, HolderStrength: 0.7, ThesisBreadth: 0.5},
	}
	v := New(Config{}).Decide(in)
	if v.WinningSide != "NO" {
		t.Errorf("MM-like A must lose; got %+v (dominance=%.2f)", v, v.Dominance)
	}
}
