package walletcohort

import (
	"testing"
	"time"
)

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

// TestDecide_FreshWalletBurstFiresWithoutEdges proves the v11.10
// fresh-wallet branch: brand-new wallets converging same-side fire
// the booster even with no historical co-trade edges. The boost is
// capped at MaxBoost/2 because the structural signal is weaker.
func TestDecide_FreshWalletBurstFiresWithoutEdges(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	in := Input{
		AlertWallet:    "A",
		AlertSide:      "YES",
		AlertEnteredAt: now.Add(-30 * time.Minute),
		Now:            now,
		Edges:          nil, // explicitly no historical edges
		RecentMembers: []CohortMember{
			// 3 fresh wallets first_seen ≤ 24h, same side
			{Wallet: "B", Side: "YES", FirstSeenAt: now.Add(-2 * time.Hour)},
			{Wallet: "C", Side: "YES", FirstSeenAt: now.Add(-8 * time.Hour)},
			{Wallet: "D", Side: "YES", FirstSeenAt: now.Add(-18 * time.Hour)},
			// 1 stale wallet — should not count
			{Wallet: "OLD", Side: "YES", FirstSeenAt: now.Add(-72 * time.Hour)},
		},
	}
	v := New(Config{MaxBoost: 6, FreshWalletMinBurst: 3, FreshWalletMaxAge: 24 * time.Hour}).Decide(in)
	if !v.Converged {
		t.Fatalf("fresh_wallet_burst must converge; got %+v", v)
	}
	if v.BranchHit != "fresh_wallet_burst" {
		t.Fatalf("BranchHit must be fresh_wallet_burst; got %q", v.BranchHit)
	}
	if v.CohortSize < 4 {
		t.Fatalf("burst size must include 3 fresh peers + alert wallet (=4); got %d", v.CohortSize)
	}
	if v.Boost <= 0 || v.Boost > 3.0 {
		t.Fatalf("fresh-branch boost must be in (0, MaxBoost/2=3]; got %v", v.Boost)
	}
}

// TestDecide_FreshBurstRejectsStaleWallets proves we don't fire when
// the burst includes stale wallets that drift through the recent
// window.
func TestDecide_FreshBurstRejectsStaleWallets(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	in := Input{
		AlertWallet:    "A",
		AlertSide:      "YES",
		AlertEnteredAt: now.Add(-72 * time.Hour), // alert wallet is stale
		Now:            now,
		Edges:          nil,
		RecentMembers: []CohortMember{
			{Wallet: "OLD1", Side: "YES", FirstSeenAt: now.Add(-72 * time.Hour)},
			{Wallet: "OLD2", Side: "YES", FirstSeenAt: now.Add(-100 * time.Hour)},
		},
	}
	v := New(Config{FreshWalletMinBurst: 3, FreshWalletMaxAge: 24 * time.Hour}).Decide(in)
	if v.Converged {
		t.Fatalf("must not converge — no fresh wallets in burst; got %+v", v)
	}
}

// TestDecide_FreshBurstSkippedWhenNowZero proves the fresh branch is
// only evaluated when orchestration passes Now (defensive against
// callers that don't fill the field).
func TestDecide_FreshBurstSkippedWhenNowZero(t *testing.T) {
	in := Input{
		AlertWallet: "A",
		AlertSide:   "YES",
		Edges:       nil,
		RecentMembers: []CohortMember{
			{Wallet: "B", Side: "YES", FirstSeenAt: time.Unix(1, 0)},
			{Wallet: "C", Side: "YES", FirstSeenAt: time.Unix(2, 0)},
			{Wallet: "D", Side: "YES", FirstSeenAt: time.Unix(3, 0)},
		},
	}
	v := New(Config{FreshWalletMinBurst: 3}).Decide(in)
	if v.Converged {
		t.Fatalf("Now zero must disable fresh branch; got %+v", v)
	}
}
