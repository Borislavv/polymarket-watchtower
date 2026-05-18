package ownership

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// shares is a small helper for readable test setup.
func shares(walletBuy, walletSell, marketBuy, priceHint float64) Shares {
	return Shares{
		WalletBuyShares:  walletBuy,
		WalletSellShares: walletSell,
		MarketBuyShares:  marketBuy,
		PriceHint:        priceHint,
	}
}

// TestSharePct_ZeroDenominatorDoesNotPanic pins the floor case — an
// outcome with no recorded BUYs (the denominator) returns 0 rather
// than NaN/Inf. Shouldn't happen in production (we only invoke this
// alongside accumulation, which by definition implies trades), but the
// math layer must be safe.
func TestSharePct_ZeroDenominatorDoesNotPanic(t *testing.T) {
	if got := shares(100, 0, 0, 0.5).SharePct(); got != 0 {
		t.Errorf("zero denominator: got %v want 0", got)
	}
}

// TestDecide_BelowFloorNoFire confirms the absolute-position floor:
// a wallet that "owns 50%" of a dust market never fires.
func TestDecide_BelowFloorNoFire(t *testing.T) {
	// 5000 net shares × $0.10 = $500 notional — below default $10k floor.
	v := New(Config{Enabled: true}).Decide(shares(5000, 0, 10000, 0.10))
	if v.Fired {
		t.Fatalf("must not fire below MinNotionalUSD: %+v", v)
	}
	// Diagnostic SharePct still populated.
	if v.SharePct != 50 {
		t.Errorf("SharePct should still be computed for diagnostics: got %v", v.SharePct)
	}
}

// TestDecide_TierLadder pins the three tiers fire at the right
// percentages and stop at the lower one when applicable.
func TestDecide_TierLadder(t *testing.T) {
	cfg := Config{Enabled: true, InfoPct: 10, WarningPct: 15, CriticalPct: 25, MinNotionalUSD: 1_000}
	d := New(cfg)

	cases := []struct {
		name     string
		s        Shares
		wantFire bool
		wantSev  anomaly.Severity
	}{
		// Just below Info — 9.9% of 100,000 share market, net=9900, $4950 notional > $1k floor.
		{"9.9% — no fire", shares(9_900, 0, 100_000, 0.50), false, ""},
		// 10% — Info.
		{"10% — Info", shares(10_000, 0, 100_000, 0.50), true, anomaly.SeverityInfo},
		// 14.99% — Info (just below Warning).
		{"14.99% — Info", shares(14_990, 0, 100_000, 0.50), true, anomaly.SeverityInfo},
		// 15% — Warning.
		{"15% — Warning", shares(15_000, 0, 100_000, 0.50), true, anomaly.SeverityWarning},
		// 24.99% — Warning.
		{"24.99% — Warning", shares(24_990, 0, 100_000, 0.50), true, anomaly.SeverityWarning},
		// 25% — Critical.
		{"25% — Critical", shares(25_000, 0, 100_000, 0.50), true, anomaly.SeverityCritical},
		// 50% — Critical.
		{"50% — Critical", shares(50_000, 0, 100_000, 0.50), true, anomaly.SeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := d.Decide(tc.s)
			if v.Fired != tc.wantFire {
				t.Errorf("fired: got %v want %v (pct=%.2f)", v.Fired, tc.wantFire, v.SharePct)
			}
			if v.Severity != tc.wantSev {
				t.Errorf("severity: got %s want %s", v.Severity, tc.wantSev)
			}
		})
	}
}

// TestDecide_DisabledNeverFires confirms the master switch off-path.
func TestDecide_DisabledNeverFires(t *testing.T) {
	v := New(Config{Enabled: false}).Decide(shares(50_000, 0, 100_000, 0.50))
	if v.Fired {
		t.Fatalf("Enabled=false must never fire: %+v", v)
	}
}

// TestDecide_SellsReduceNetShares confirms a wallet that bought 30%
// then sold most of it does NOT fire — net is what matters, not
// gross BUY volume.
func TestDecide_SellsReduceNetShares(t *testing.T) {
	cfg := Config{Enabled: true, InfoPct: 10, WarningPct: 15, CriticalPct: 25, MinNotionalUSD: 1_000}
	d := New(cfg)
	// Bought 30k shares, sold 25k → net 5k → 5% share — no fire.
	v := d.Decide(shares(30_000, 25_000, 100_000, 0.50))
	if v.Fired {
		t.Fatalf("net-share semantics violated — wallet sold back, must not fire: %+v", v)
	}
	if v.NetShares != 5_000 {
		t.Errorf("NetShares: got %v want 5000", v.NetShares)
	}
}

// TestDecide_CriticalCarriesDominateReason pins the reason-code
// emission shape: Critical attaches WALLET_DOMINATES_OUTCOME on top of
// the base concentration reason.
func TestDecide_CriticalCarriesDominateReason(t *testing.T) {
	d := New(Config{Enabled: true, InfoPct: 10, WarningPct: 15, CriticalPct: 25, MinNotionalUSD: 1_000})
	v := d.Decide(shares(30_000, 0, 100_000, 0.50))
	if !v.Fired || v.Severity != anomaly.SeverityCritical {
		t.Fatalf("expected Critical fire, got %+v", v)
	}
	seen := map[ReasonCode]bool{}
	for _, r := range v.Reasons {
		seen[r] = true
	}
	if !seen[ReasonConcentration] || !seen[ReasonDominates] {
		t.Fatalf("Critical must carry both concentration + dominate reasons, got %v", v.Reasons)
	}
}
