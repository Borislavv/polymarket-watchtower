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

func bs(count int, median, total float64) baseline.Stats {
	return baseline.Stats{Count: count, MedianUSD: median, MeanUSD: median, TotalUSD: total}
}

// price = 1/odds — quick helper so tests read naturally.
func priceFor(odds float64) float64 { return 1.0 / odds }

func TestBelowAbsoluteFloorNoAlert(t *testing.T) {
	th := defaults()
	// $5k at odds 10, baseline meets multiplier (5000/10=500) — but notional too small.
	r := Score(5_000, priceFor(10), bs(200, 10, 10_000), th)
	if r.Fired {
		t.Fatalf("expected no fire below absolute floor: %+v", r)
	}
}

func TestBelowMultiplierFloorNoAlert(t *testing.T) {
	th := defaults()
	// $10k at odds 3, baseline median $200 → multiplier 50, below 100 → no fire.
	r := Score(10_000, priceFor(3), bs(200, 200, 50_000), th)
	if r.Fired {
		t.Fatalf("expected no fire below multiplier floor: %+v", r)
	}
}

func TestInfoTier(t *testing.T) {
	th := defaults()
	// $10k at odds 3 with baseline median $50 → multiplier 200 → info on both → info.
	r := Score(10_000, priceFor(3), bs(200, 50, 10_000), th)
	if !r.Fired || r.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected info, got %+v", r)
	}
	if r.AbsoluteTier != anomaly.SeverityInfo || r.MultiplierTier != anomaly.SeverityInfo {
		t.Fatalf("tier composition: %+v", r)
	}
}

func TestWarningTier(t *testing.T) {
	th := defaults()
	// $25k at odds 5, baseline median $20 → multiplier 1250 → both warning.
	r := Score(25_000, priceFor(5), bs(200, 20, 20_000), th)
	if !r.Fired || r.Severity != anomaly.SeverityWarning {
		t.Fatalf("expected warning, got %+v", r)
	}
}

func TestCriticalTier(t *testing.T) {
	th := defaults()
	// $100k at odds 8, baseline median $9 → multiplier ≈ 11_111 → both critical.
	r := Score(100_000, priceFor(8), bs(500, 9, 50_000), th)
	if !r.Fired || r.Severity != anomaly.SeverityCritical {
		t.Fatalf("expected critical, got %+v", r)
	}
}

func TestConservativeMinPicksLowerTier(t *testing.T) {
	th := defaults()
	// $120k at odds 9 — absolute=critical.
	// baseline median $800 → multiplier 150 — multiplier=info.
	// final must be info (lower wins).
	r := Score(120_000, priceFor(9), bs(200, 800, 200_000), th)
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

func TestInsufficientBaselineNoAlert(t *testing.T) {
	th := defaults()
	// Absolute fires (info), but baseline has only 5 samples → multiplier path
	// skipped → no fire (strict).
	r := Score(10_000, priceFor(3), bs(5, 10, 50), th)
	if r.Fired {
		t.Fatalf("expected no fire on thin baseline: %+v", r)
	}
	if r.AbsoluteTier != anomaly.SeverityInfo {
		t.Errorf("absolute tier should still be computed: %s", r.AbsoluteTier)
	}
}

func TestLowBaselineNotionalNoAlert(t *testing.T) {
	th := defaults()
	// 200 samples but total $5 — baseline is dust, no alert.
	r := Score(10_000, priceFor(3), bs(200, 0.05, 5), th)
	if r.Fired {
		t.Fatalf("expected no fire on dust baseline: %+v", r)
	}
}

func TestZeroPriceNoPanic(t *testing.T) {
	if r := Score(10_000, 0, baseline.Stats{}, defaults()); r.Fired {
		t.Fatal("zero price must not fire")
	}
}

func TestNonPositiveNotionalIgnored(t *testing.T) {
	if r := Score(-1, 0.5, bs(100, 10, 1_000), defaults()); r.Fired {
		t.Fatal("negative notional must not fire")
	}
}

func TestRequiresBothNotionalAndOdds(t *testing.T) {
	th := defaults()
	// $50k at odds 2 (price 0.5) — notional ≥ warning, odds < info — no fire.
	r := Score(50_000, 0.5, bs(500, 5, 10_000), th)
	if r.Fired {
		t.Fatalf("expected no fire when odds too low: %+v", r)
	}
}
