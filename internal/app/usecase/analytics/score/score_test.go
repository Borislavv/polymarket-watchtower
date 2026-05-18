package score

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

func defaults() anomaly.Thresholds {
	return anomaly.Thresholds{
		Info:                   anomaly.Tier{MinNotionalUSD: 10_000, MinOdds: 3, MinMultiplier: 100},
		Warning:                anomaly.Tier{MinNotionalUSD: 25_000, MinOdds: 5, MinMultiplier: 1_000},
		Critical:               anomaly.Tier{MinNotionalUSD: 100_000, MinOdds: 8, MinMultiplier: 10_000},
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}
}

// bs builds a market baseline with the supplied count + median; total is
// derived as count*median so the readiness gate behaves intuitively.
func bs(count int, median float64) baseline.Stats {
	total := median * float64(count)
	if total < 1_000 {
		// keep above the test default MinBaselineNotionalUSD so callers that
		// want a "thin baseline" can express it via Count instead.
		total = float64(count) * median
	}
	return baseline.Stats{Count: count, MedianUSD: median, MeanUSD: median, TotalUSD: total}
}

// noTrader is the explicit "trader axis disabled" sentinel.
var noTrader = baseline.Stats{}

func priceFor(odds float64) float64 { return 1.0 / odds }

func TestBelowAbsoluteFloorNoAlert(t *testing.T) {
	r := Score(5_000, priceFor(10), bs(200, 10), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire below absolute floor: %+v", r)
	}
}

func TestBelowMultiplierFloorNoAlert(t *testing.T) {
	// $10k at odds 3, market median $200 → multiplier 50, below 100 → no fire.
	r := Score(10_000, priceFor(3), bs(200, 200), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire below multiplier floor: %+v", r)
	}
}

func TestInfoTier_MarketOnly(t *testing.T) {
	// $10k at odds 3, market median $50 → mult 200 → info × info → info.
	r := Score(10_000, priceFor(3), bs(200, 50), noTrader, defaults())
	if !r.Fired || r.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected info, got %+v", r)
	}
	if r.MultiplierAxis != MultiplierAxisMarket {
		t.Errorf("expected market axis, got %q", r.MultiplierAxis)
	}
}

func TestWarningTier_MarketOnly(t *testing.T) {
	r := Score(25_000, priceFor(5), bs(200, 20), noTrader, defaults())
	if !r.Fired || r.Severity != anomaly.SeverityWarning {
		t.Fatalf("expected warning, got %+v", r)
	}
}

func TestCriticalTier_MarketOnly(t *testing.T) {
	r := Score(100_000, priceFor(8), bs(500, 9), noTrader, defaults())
	if !r.Fired || r.Severity != anomaly.SeverityCritical {
		t.Fatalf("expected critical, got %+v", r)
	}
}

func TestConservativeMinPicksLowerTier(t *testing.T) {
	// $120k at odds 9 — absolute=critical.
	// market median $800 → mult 150 → multiplier=info.
	// final = info (lower wins, regardless of axes).
	r := Score(120_000, priceFor(9), bs(200, 800), noTrader, defaults())
	if !r.Fired {
		t.Fatal("expected fire")
	}
	if r.AbsoluteTier != anomaly.SeverityCritical {
		t.Errorf("absolute tier: %s", r.AbsoluteTier)
	}
	if r.MultiplierTier != anomaly.SeverityInfo {
		t.Errorf("multiplier tier: %s", r.MultiplierTier)
	}
	if r.Severity != anomaly.SeverityInfo {
		t.Fatalf("final severity must be the lower one: got %s", r.Severity)
	}
}

func TestInsufficientMarketBaselineNoAlert(t *testing.T) {
	// Absolute fires; market baseline has only 5 samples → market axis disabled.
	// No trader axis either → no fire (preserves v1 contract).
	r := Score(10_000, priceFor(3), bs(5, 10), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire on thin baseline: %+v", r)
	}
	if r.AbsoluteTier != anomaly.SeverityInfo {
		t.Errorf("absolute tier should still be computed: %s", r.AbsoluteTier)
	}
}

func TestZeroPriceNoPanic(t *testing.T) {
	if r := Score(10_000, 0, baseline.Stats{}, noTrader, defaults()); r.Fired {
		t.Fatal("zero price must not fire")
	}
}

func TestNonPositiveNotionalIgnored(t *testing.T) {
	if r := Score(-1, 0.5, bs(100, 10), noTrader, defaults()); r.Fired {
		t.Fatal("negative notional must not fire")
	}
}

func TestRequiresBothNotionalAndOdds(t *testing.T) {
	// $50k at odds 2 (price 0.5) — notional ≥ warning, odds < info → no fire.
	r := Score(50_000, 0.5, bs(500, 5), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire when odds too low: %+v", r)
	}
}

// --- trader-axis path -------------------------------------------------------

// TestTraderAxis_SmallWalletHugeBetFires is the canonical informed-flow
// shape that v1 missed: a wallet whose own history is $200 / trade puts up
// $25k. Market is busy (median $5k) so the market axis says mult=5 (no
// rung), but the trader axis says mult=125 → Info. Absolute clears Info.
// → Fire at Info with axis="trader".
func TestTraderAxis_SmallWalletHugeBetFires(t *testing.T) {
	th := defaults()
	market := bs(200, 5_000) // busy market: mult on $25k = 5 → no rung
	trader := bs(40, 200)    // $200 typical: mult on $25k = 125 → info rung

	r := Score(25_000, priceFor(5), market, trader, th)
	if !r.Fired {
		t.Fatalf("expected fire on small-wallet-huge-bet: %+v", r)
	}
	if r.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected info severity, got %s", r.Severity)
	}
	if r.MultiplierAxis != MultiplierAxisTrader {
		t.Errorf("expected trader axis, got %q", r.MultiplierAxis)
	}
	if r.TraderMultiplier < 120 || r.TraderMultiplier > 130 {
		t.Errorf("trader multiplier: %v want ~125", r.TraderMultiplier)
	}
	if r.MarketMultiplier > 10 {
		t.Errorf("market multiplier should be small: %v", r.MarketMultiplier)
	}
}

// TestTraderAxis_LargeWhaleNoFire pins the FP suppression: a whale that
// routinely does $20k trades does another $25k — market multiplier 5 (no
// rung), trader multiplier 1.25 (no rung) → no fire.
func TestTraderAxis_LargeWhaleNoFire(t *testing.T) {
	th := defaults()
	market := bs(200, 5_000) // mult = 5 → no rung
	trader := bs(80, 20_000) // mult = 1.25 → no rung

	r := Score(25_000, priceFor(5), market, trader, th)
	if r.Fired {
		t.Fatalf("expected no fire — neither axis is anomalous: %+v", r)
	}
}

// TestTraderAxis_DisabledFallsBackToMarket pins backwards compatibility: a
// trade with no trader-axis (wallet unknown or too new) behaves exactly
// like v1 (market-only scoring).
func TestTraderAxis_DisabledFallsBackToMarket(t *testing.T) {
	th := defaults()
	market := bs(200, 50) // mult = 200 → info rung

	r := Score(10_000, priceFor(3), market, noTrader, th)
	if !r.Fired || r.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected info from market axis: %+v", r)
	}
	if r.MultiplierAxis != MultiplierAxisMarket {
		t.Errorf("axis: %q", r.MultiplierAxis)
	}
	if r.TraderMultiplier != 0 {
		t.Errorf("trader multiplier should be 0 with empty stats: %v", r.TraderMultiplier)
	}
}

// TestTraderAxis_BothEqualReportsBoth pins the edge case where the trade
// is equally anomalous on both axes.
func TestTraderAxis_BothEqualReportsBoth(t *testing.T) {
	th := defaults()
	market := bs(200, 100) // mult = 100
	trader := bs(40, 100)  // mult = 100

	r := Score(10_000, priceFor(3), market, trader, th)
	if !r.Fired {
		t.Fatal("expected fire")
	}
	if r.MultiplierAxis != MultiplierAxisBoth {
		t.Errorf("axis: %q want both", r.MultiplierAxis)
	}
}

// TestTraderAxis_MarketUnreadyTraderFires pins the v2 capability that v1
// lacked: when the market baseline is unready (cold start) but the trader
// has rich history, the trade can still fire on the trader axis alone.
func TestTraderAxis_MarketUnreadyTraderFires(t *testing.T) {
	th := defaults()
	market := bs(3, 5_000) // 3 samples → market axis disabled
	trader := bs(50, 100)  // mult on $10k = 100 → info

	r := Score(10_000, priceFor(3), market, trader, th)
	if !r.Fired {
		t.Fatalf("expected fire from trader-only axis: %+v", r)
	}
	if r.MultiplierAxis != MultiplierAxisTrader {
		t.Errorf("axis: %q want trader", r.MultiplierAxis)
	}
	if r.MarketMultiplier != 0 {
		t.Errorf("market multiplier should be 0 when market unready: %v", r.MarketMultiplier)
	}
}

// TestTraderAxis_PicksHigherMultiplier pins the max() semantics: when both
// axes are ready, the higher (more anomalous) multiplier drives the tier.
func TestTraderAxis_PicksHigherMultiplier(t *testing.T) {
	th := defaults()
	market := bs(200, 50) // mult on $25k = 500 → info rung (>100)
	// 40 trades × $30 = $1200 total > MinBaselineNotionalUSD; mult $25k/$30 = 833 → no rung.
	// Bump to 50 trades × $25 = $1250; mult $25k/$25 = 1000 → exactly warning rung.
	trader := bs(50, 20) // mult on $25k = 1250 → warning rung

	r := Score(25_000, priceFor(5), market, trader, th)
	if !r.Fired {
		t.Fatal("expected fire")
	}
	if r.MultiplierTier != anomaly.SeverityWarning {
		t.Errorf("expected warning tier from trader axis, got %s", r.MultiplierTier)
	}
	if r.MultiplierAxis != MultiplierAxisTrader {
		t.Errorf("expected trader axis, got %q", r.MultiplierAxis)
	}
	if r.Severity != anomaly.SeverityWarning {
		t.Errorf("severity: got %s want warning", r.Severity)
	}
}
