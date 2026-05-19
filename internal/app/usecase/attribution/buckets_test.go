package attribution

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// TestStrategyFamily pins the per-Kind family value. A drift here
// breaks every dashboard that groups by strategy_family.
func TestStrategyFamily(t *testing.T) {
	cases := []struct {
		kind anomaly.Kind
		want string
	}{
		{anomaly.KindTradeAnomaly, "whale_flow"},
		{anomaly.KindAccumulation, "accumulation"},
		{anomaly.KindOwnership, "ownership_concentration"},
		{anomaly.KindStableFavorite, "stable_favorite"},
		{anomaly.KindCategoryWatch, "category_cluster"},
	}
	for _, c := range cases {
		got := strategyFamily(anomaly.Finding{Kind: c.kind})
		if got != c.want {
			t.Errorf("kind=%q got=%q want=%q", c.kind, got, c.want)
		}
	}
}

// TestLifecycleBucket pins the 5% bands at and around the alerting
// threshold. Off-by-one here would shift every panel.
func TestLifecycleBucket(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, "0-75%"}, {74.9, "0-75%"},
		{75, "75-80%"}, {79.9, "75-80%"},
		{80, "80-85%"},
		{85, "85-90%"},
		{90, "90-95%"},
		{95, "95-100%"}, {100, "95-100%"},
	}
	for _, c := range cases {
		got := lifecycleBucket(c.pct)
		if got != c.want {
			t.Errorf("pct=%.2f got=%q want=%q", c.pct, got, c.want)
		}
	}
}

func TestNotionalBucket(t *testing.T) {
	cases := []struct {
		usd  float64
		want string
	}{
		{0, ""}, {500, "<1k"}, {1_500, "1-5k"}, {7_000, "5-10k"},
		{15_000, "10-25k"}, {50_000, "25-100k"}, {250_000, "100-500k"},
		{1_000_000, "500k+"},
	}
	for _, c := range cases {
		if got := notionalBucket(c.usd); got != c.want {
			t.Errorf("usd=%.0f got=%q want=%q", c.usd, got, c.want)
		}
	}
}

func TestOddsBucket(t *testing.T) {
	cases := []struct {
		odds float64
		want string
	}{
		{0, ""}, {1.5, "<2x"}, {2.5, "2-3x"}, {4, "3-5x"},
		{7, "5-10x"}, {15, "10-25x"}, {50, "25-100x"}, {200, "100x+"},
	}
	for _, c := range cases {
		if got := oddsBucket(c.odds); got != c.want {
			t.Errorf("odds=%.2f got=%q want=%q", c.odds, got, c.want)
		}
	}
}

func TestReturnBucketFromPrice(t *testing.T) {
	// (1-p)/p × 100 → 0.5→100% (band 50-100%); 0.8→25% (band 10-25%);
	// 0.2→400% (band 100-500%); 0.95→5.26% (band 0-10%).
	cases := []struct {
		price float64
		want  string
	}{
		{0, ""}, {1, ""}, {1.1, ""},
		{0.51, "50-100%"}, // (1-0.51)/0.51 ≈ 96%
		{0.8, "10-25%"},
		{0.2, "100-500%"},
		{0.95, "0-10%"},
		{0.1, "500%+"},
	}
	for _, c := range cases {
		if got := returnBucketFromPrice(c.price); got != c.want {
			t.Errorf("price=%.2f got=%q want=%q", c.price, got, c.want)
		}
	}
}

func TestOwnershipShareBucket(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, ""}, {0.5, "<1%"}, {3, "1-5%"}, {8, "5-10%"},
		{20, "10-25%"}, {40, "25-50%"}, {75, "50%+"},
	}
	for _, c := range cases {
		if got := ownershipShareBucket(c.pct); got != c.want {
			t.Errorf("pct=%.2f got=%q want=%q", c.pct, got, c.want)
		}
	}
}

func TestVolatilityRegime(t *testing.T) {
	cases := []struct {
		stddev float64
		want   string
	}{
		{0, ""}, {0.005, "very_low"}, {0.02, "low"}, {0.04, "moderate"},
		{0.08, "elevated"}, {0.2, "high"},
	}
	for _, c := range cases {
		if got := volatilityRegimeFromStddev(c.stddev); got != c.want {
			t.Errorf("stddev=%.3f got=%q want=%q", c.stddev, got, c.want)
		}
	}
}

// TestFromFinding_WhaleFlow pins a whole row mapping for a typical
// large-bet alert. The Telegram body and the dashboards both consume
// this row — keep it locked.
func TestFromFinding_WhaleFlow(t *testing.T) {
	f := anomaly.Finding{
		Kind:         anomaly.KindTradeAnomaly,
		Severity:     anomaly.SeverityCritical,
		LifecyclePct: 92,
		Trade: &anomaly.TradeRef{
			Odds:        7,
			NotionalUSD: 50_000,
			Price:       0.6, // remaining return ≈ 66.7%
		},
		Category: &anomaly.CategoryRef{Slug: "Politics"},
		NewWallet: &anomaly.NewWalletRef{IsNew: true},
	}
	got := FromFinding(101, f, "lean_yes")
	if got.AlertID != 101 {
		t.Fatalf("alert id: got %d want 101", got.AlertID)
	}
	if got.StrategyFamily != "whale_flow" {
		t.Errorf("family: %q", got.StrategyFamily)
	}
	if got.LifecycleBucket != "90-95%" {
		t.Errorf("lifecycle: %q", got.LifecycleBucket)
	}
	if got.OddsBucket != "5-10x" {
		t.Errorf("odds: %q", got.OddsBucket)
	}
	if got.NotionalBucket != "25-100k" {
		t.Errorf("notional: %q", got.NotionalBucket)
	}
	if got.ReturnBucket != "50-100%" {
		t.Errorf("return: %q", got.ReturnBucket)
	}
	if got.Category != "politics" {
		t.Errorf("category: %q", got.Category)
	}
	if !got.NewWallet {
		t.Error("new_wallet flag should be true")
	}
	if got.AIVerdict != "lean_yes" {
		t.Errorf("ai_verdict: %q", got.AIVerdict)
	}
}

// TestFromFinding_Accumulation pins the accumulation family. The
// window column ("recent" vs "lifetime") is the distinguishing axis
// for the "many-smalls vs one-shot" decomposition.
func TestFromFinding_Accumulation(t *testing.T) {
	f := anomaly.Finding{
		Kind:         anomaly.KindAccumulation,
		LifecyclePct: 88,
		Accumulation: &anomaly.AccumulationRef{
			AvgOdds:          4,
			TotalNotionalUSD: 12_000,
			Window:           "lifetime",
		},
	}
	got := FromFinding(7, f, "")
	if got.StrategyFamily != "accumulation" {
		t.Errorf("family: %q", got.StrategyFamily)
	}
	if got.OddsBucket != "3-5x" {
		t.Errorf("odds: %q", got.OddsBucket)
	}
	if got.NotionalBucket != "10-25k" {
		t.Errorf("notional: %q", got.NotionalBucket)
	}
	if got.AccumulationWindow != "lifetime" {
		t.Errorf("window: %q", got.AccumulationWindow)
	}
	if got.LifecycleBucket != "85-90%" {
		t.Errorf("lifecycle: %q", got.LifecycleBucket)
	}
}

func TestFromFinding_Ownership(t *testing.T) {
	f := anomaly.Finding{
		Kind: anomaly.KindOwnership,
		Ownership: &anomaly.OwnershipRef{
			SharePct:    8,
			NotionalUSD: 30_000,
		},
		LifecyclePct: 95,
	}
	got := FromFinding(9, f, "")
	if got.StrategyFamily != "ownership_concentration" {
		t.Errorf("family: %q", got.StrategyFamily)
	}
	if got.OwnershipShareBucket != "5-10%" {
		t.Errorf("share: %q", got.OwnershipShareBucket)
	}
	if got.NotionalBucket != "25-100k" {
		t.Errorf("notional: %q", got.NotionalBucket)
	}
}

func TestFromFinding_StableFavorite(t *testing.T) {
	f := anomaly.Finding{
		Kind: anomaly.KindStableFavorite,
		StableFavorite: &anomaly.StableFavoriteRef{
			Probability:        0.7, // 1/0.7 ≈ 1.43 → odds bucket "<2x"
			RemainingReturnPct: 42.86,
			PriceStddev:        0.015,
		},
		LifecyclePct: 96,
	}
	got := FromFinding(3, f, "")
	if got.StrategyFamily != "stable_favorite" {
		t.Errorf("family: %q", got.StrategyFamily)
	}
	if got.OddsBucket != "<2x" {
		t.Errorf("odds: %q", got.OddsBucket)
	}
	if got.ReturnBucket != "25-50%" {
		t.Errorf("return: %q", got.ReturnBucket)
	}
	if got.VolatilityRegime != "low" {
		t.Errorf("volatility: %q", got.VolatilityRegime)
	}
}

func TestFromFinding_CategoryWatchCluster(t *testing.T) {
	f := anomaly.Finding{
		Kind: anomaly.KindCategoryWatch,
		Cluster: &anomaly.ClusterStats{TotalUSD: 75_000},
	}
	got := FromFinding(2, f, "")
	if got.StrategyFamily != "category_cluster" {
		t.Errorf("family: %q", got.StrategyFamily)
	}
	if got.NotionalBucket != "25-100k" {
		t.Errorf("notional: %q", got.NotionalBucket)
	}
}

// TestFromFinding_BooleanFlags pins the three booleans the dashboards
// use as filter pills.
func TestFromFinding_BooleanFlags(t *testing.T) {
	f := anomaly.Finding{
		Kind:          anomaly.KindTradeAnomaly,
		Trade:         &anomaly.TradeRef{},
		NewWallet:     &anomaly.NewWalletRef{IsNew: false},
		QuietMarket:   &anomaly.QuietMarketRef{},
		DormantWallet: &anomaly.DormantWalletRef{},
	}
	got := FromFinding(1, f, "")
	if got.NewWallet {
		t.Error("new_wallet=false (IsNew=false) must stamp false")
	}
	if !got.QuietMarket {
		t.Error("quiet_market present → flag must be true")
	}
	if !got.DormantWallet {
		t.Error("dormant_wallet present → flag must be true")
	}
}
