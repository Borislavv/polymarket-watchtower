package walletcohort

import "testing"

func TestDecide_PeersConvergeOnSameSideFiresBoost(t *testing.T) {
	in := Input{
		AlertWallet: "A",
		AlertSide:   "YES",
		Edges: []Edge{
			{WalletA: "A", WalletB: "B", SimilarityScore: 0.8, CoEventsCount: 5, CohortID: "c1"},
			{WalletA: "A", WalletB: "C", SimilarityScore: 0.7, CoEventsCount: 4, CohortID: "c1"},
		},
		RecentMembers: []CohortMember{
			{Wallet: "B", Side: "YES"},
			{Wallet: "C", Side: "YES"},
			{Wallet: "Z", Side: "NO"}, // unrelated
		},
	}
	v := New(Config{}).Decide(in)
	if !v.Converged {
		t.Fatalf("expected converged; got %+v", v)
	}
	if v.CohortSize != 3 {
		t.Errorf("cohort size should be 3 (A+B+C); got %d", v.CohortSize)
	}
	if v.Boost <= 0 {
		t.Errorf("boost must be positive; got %v", v.Boost)
	}
}

func TestDecide_LowSimilarityNoBoost(t *testing.T) {
	in := Input{
		AlertWallet: "A",
		AlertSide:   "YES",
		Edges: []Edge{
			{WalletA: "A", WalletB: "B", SimilarityScore: 0.3, CoEventsCount: 5},
		},
		RecentMembers: []CohortMember{{Wallet: "B", Side: "YES"}},
	}
	v := New(Config{}).Decide(in)
	if v.Converged {
		t.Fatalf("must not converge under low similarity; got %+v", v)
	}
}

func TestDecide_NoConvergenceWhenPeersOnOtherSide(t *testing.T) {
	in := Input{
		AlertWallet: "A",
		AlertSide:   "YES",
		Edges: []Edge{
			{WalletA: "A", WalletB: "B", SimilarityScore: 0.9, CoEventsCount: 6},
		},
		RecentMembers: []CohortMember{{Wallet: "B", Side: "NO"}},
	}
	v := New(Config{}).Decide(in)
	if v.Converged {
		t.Fatalf("must not converge when peer on opposite side; got %+v", v)
	}
}
