package score

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// defaults returns the v5 tail+payoff threshold layout. The numbers are the
// shape used in production envs; specific tests override individual fields.
func defaults() anomaly.Thresholds {
	return anomaly.Thresholds{
		Info: anomaly.Tier{
			MinNotionalUSD:    10_000,
			MinOdds:           3,
			MinProfitUSD:      5_000,
			MinMarketP95Ratio: 1,
			MinTraderP95Ratio: 1,
		},
		Warning: anomaly.Tier{
			MinNotionalUSD:    25_000,
			MinOdds:           5,
			MinProfitUSD:      15_000,
			MinMarketP95Ratio: 2,
			MinTraderP95Ratio: 1.5,
		},
		Critical: anomaly.Tier{
			MinNotionalUSD:    100_000,
			MinOdds:           8,
			MinProfitUSD:      50_000,
			MinMarketP95Ratio: 4,
			MinTraderP95Ratio: 2,
		},
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}
}

// market is a market-baseline builder. Caller specifies count, median, p95,
// p99 directly so each test reflects the shape under test. TotalUSD is
// derived as count*median (a sane approximation of "ready enough").
func market(count int, median, p95, p99 float64) baseline.Stats {
	total := median * float64(count)
	if total < 1_000 {
		total = 1_000
	}
	return baseline.Stats{
		Count:     count,
		MedianUSD: median,
		MeanUSD:   median,
		P95USD:    p95,
		P99USD:    p99,
		TotalUSD:  total,
	}
}

// trader builds a trader-history baseline. Same shape as market but with no
// MinBaselineNotionalUSD coupling (the trader axis ignores TotalUSD).
func trader(count int, median, p95, p99 float64) baseline.Stats {
	return baseline.Stats{
		Count:     count,
		MedianUSD: median,
		MeanUSD:   median,
		P95USD:    p95,
		P99USD:    p99,
		TotalUSD:  median * float64(count),
	}
}

var noTrader = baseline.Stats{}
var noMarket = baseline.Stats{}

func priceFor(odds float64) float64 { return 1.0 / odds }

// --- The canonical false-positive from the task brief ----------------------

// TestFalsePositive_BelowTraderP95_DoesNotFire pins the headline case the
// refactor exists to fix. A $1956 bet at odds 2.86 sits well above the
// market median ($6.31), so v4 multiplier-driven scoring fired Info.
// Under v5 the trade fails BOTH the payoff floor (profit≈$3.6k < $5k) and
// the trader p95 gate (0.65× < 1.0× required) → no fire.
func TestFalsePositive_BelowTraderP95_DoesNotFire(t *testing.T) {
	r := Score(1_956, 1.0/2.86,
		market(250, 6.31, 436, 3_832),
		trader(80, 240, 3_004, 9_723),
		defaults())
	if r.Fired {
		t.Fatalf("expected no fire on the canonical false-positive shape: %+v", r)
	}
	if r.SuppressedReason == "" {
		t.Fatalf("expected a suppression reason on a non-fire: %+v", r)
	}
	// Payoff and tail ratios are populated even on a suppression so a
	// debug logger can read the reasoning without re-running scoring.
	if r.TraderP95Ratio < 0.6 || r.TraderP95Ratio > 0.7 {
		t.Errorf("trader p95 ratio: got %.3f want ~0.65", r.TraderP95Ratio)
	}
	if r.ProfitIfWinUSD < 3_500 || r.ProfitIfWinUSD > 3_700 {
		t.Errorf("profit if win: got $%.0f want ~$3,640", r.ProfitIfWinUSD)
	}
}

// --- Positive paths --------------------------------------------------------

// TestTailAndPayoff_RealSignalFires is the "true positive" twin: a $6k bet
// at odds 3, market p95 $436, trader p95 $3000, profit $12k. Every gate
// the Info tier cares about clears.
func TestTailAndPayoff_RealSignalFires(t *testing.T) {
	r := Score(12_000, 1.0/3,
		market(200, 50, 436, 1_200),
		trader(60, 300, 3_000, 8_000),
		defaults())
	if !r.Fired {
		t.Fatalf("expected fire on real tail/payoff signal: %+v", r)
	}
	if r.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected info severity, got %s (%+v)", r.Severity, r)
	}
	if !r.PayoffGatePassed {
		t.Error("payoff gate must be marked passed")
	}
	if !r.TailGatePassed {
		t.Error("tail gate must be marked passed when at least one tail floor was enforced")
	}
	if r.ProfitIfWinUSD < 23_999 || r.ProfitIfWinUSD > 24_001 {
		t.Errorf("profit if win: got $%.0f want ~$24,000", r.ProfitIfWinUSD)
	}
}

// TestNoTraderBaseline_FiresOnMarketAlone pins that an unknown wallet (no
// trader baseline) does not block firing — the market tail + payoff gates
// alone qualify the trade.
func TestNoTraderBaseline_FiresOnMarketAlone(t *testing.T) {
	r := Score(12_000, 1.0/3,
		market(200, 50, 436, 1_200),
		noTrader,
		defaults())
	if !r.Fired {
		t.Fatalf("expected fire when trader baseline is unavailable: %+v", r)
	}
	if r.Severity != anomaly.SeverityInfo {
		t.Fatalf("severity: got %s want info", r.Severity)
	}
	if r.TraderP95Ratio != 0 || r.TraderP99Ratio != 0 {
		t.Errorf("trader ratios must read 0 when trader baseline is empty: %+v", r)
	}
}

// TestNoMarketBaseline_FiresOnTraderAlone pins the symmetric case: a cold
// market with rich trader history. The trader tail gate alone qualifies
// the trade.
func TestNoMarketBaseline_FiresOnTraderAlone(t *testing.T) {
	r := Score(12_000, 1.0/3,
		noMarket,
		trader(60, 300, 3_000, 8_000),
		defaults())
	if !r.Fired {
		t.Fatalf("expected fire when market baseline is unavailable: %+v", r)
	}
	if r.MarketP95Ratio != 0 || r.MarketP99Ratio != 0 {
		t.Errorf("market ratios must read 0 when market baseline is empty: %+v", r)
	}
}

// TestNeitherBaselineReady_FiresOnAbsolutePayoffOnly pins the "cold start"
// path: when neither baseline has enough samples to enforce tail gates,
// the trade can still fire if the absolute + payoff floors clear. This is
// the documented "tail gate unenforced" behaviour.
func TestNeitherBaselineReady_FiresOnAbsolutePayoffOnly(t *testing.T) {
	r := Score(12_000, 1.0/3, noMarket, noTrader, defaults())
	if !r.Fired {
		t.Fatalf("expected fire on absolute+payoff alone when no baselines ready: %+v", r)
	}
	if r.TailGatePassed {
		t.Error("tail gate should NOT be marked passed when no tail floor was enforceable")
	}
}

// --- Tier escalation --------------------------------------------------------

func TestEscalatesToWarning(t *testing.T) {
	// $30k at odds 6 (price ≈ 0.167); profit ≈ $150k (clears Warning $15k).
	// Market p95 $5k → ratio 6× (≥ Warning's 2×). Trader p95 $10k →
	// ratio 3× (≥ Warning's 1.5×). Critical needs $100k notional → no.
	r := Score(30_000, 1.0/6,
		market(200, 50, 5_000, 12_000),
		trader(60, 500, 10_000, 25_000),
		defaults())
	if !r.Fired || r.Severity != anomaly.SeverityWarning {
		t.Fatalf("expected warning, got %s (%+v)", r.Severity, r)
	}
}

func TestEscalatesToCritical(t *testing.T) {
	// $300k at odds 10. Profit $2.7M (≥ Critical $50k). Market p95 $50k →
	// ratio 6× (≥ Critical 4×). Trader p95 $100k → ratio 3× (≥ Critical 2×).
	r := Score(300_000, 1.0/10,
		market(500, 500, 50_000, 200_000),
		trader(120, 8_000, 100_000, 200_000),
		defaults())
	if !r.Fired || r.Severity != anomaly.SeverityCritical {
		t.Fatalf("expected critical, got %s (%+v)", r.Severity, r)
	}
}

// --- Per-gate fail cases ----------------------------------------------------

func TestBelowAbsoluteNotional_NoFire(t *testing.T) {
	r := Score(5_000, 1.0/5, market(200, 50, 100, 300), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire below absolute floor: %+v", r)
	}
	if r.SuppressedReason != SuppressedBelowAbsolute {
		t.Errorf("expected SuppressedBelowAbsolute, got %q", r.SuppressedReason)
	}
}

func TestBelowOdds_NoFire(t *testing.T) {
	// Notional clears Info; odds 2 < Info MinOdds 3.
	r := Score(50_000, 0.5, market(200, 50, 100, 300), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire when odds below floor: %+v", r)
	}
}

func TestBelowProfit_NoFire(t *testing.T) {
	// $12k at odds 3 → profit $24k... wait need to engineer profit < $5k.
	// Notional $10k at odds 1.4 → profit $4k < Info $5k. Odds 1.4 also
	// fails MinOdds=3 — so this lands on BelowAbsolute first. To exercise
	// the payoff-only path, lower Info MinOdds to 1.0 for this test.
	th := defaults()
	th.Info.MinOdds = 1.0
	th.Warning.MinOdds = 1.0
	th.Critical.MinOdds = 1.0
	r := Score(10_000, 1.0/1.4, market(200, 50, 100, 300), noTrader, th)
	if r.Fired {
		t.Fatalf("expected no fire below payoff floor: %+v", r)
	}
	if r.SuppressedReason != SuppressedBelowPayoff {
		t.Errorf("expected SuppressedBelowPayoff, got %q", r.SuppressedReason)
	}
}

func TestBelowMarketP95_NoFire(t *testing.T) {
	// Trade $10k, market p95 $20000 → ratio 0.5 < Info 1.0.
	r := Score(10_000, 1.0/3, market(200, 50, 20_000, 50_000), noTrader, defaults())
	if r.Fired {
		t.Fatalf("expected no fire below market p95 ratio: %+v", r)
	}
	if r.SuppressedReason != SuppressedBelowMarketP95 {
		t.Errorf("expected SuppressedBelowMarketP95, got %q", r.SuppressedReason)
	}
}

func TestBelowTraderP95_NoFire(t *testing.T) {
	// Trade $10k, trader p95 $20000 → ratio 0.5 < Info 1.0.
	// Market gate passes (p95 $100).
	r := Score(10_000, 1.0/3,
		market(200, 50, 100, 300),
		trader(80, 1_000, 20_000, 40_000),
		defaults())
	if r.Fired {
		t.Fatalf("expected no fire below trader p95 ratio: %+v", r)
	}
	if r.SuppressedReason != SuppressedBelowTraderP95 {
		t.Errorf("expected SuppressedBelowTraderP95, got %q", r.SuppressedReason)
	}
}

// --- Edge cases ------------------------------------------------------------

func TestZeroPriceNoPanic(t *testing.T) {
	if r := Score(10_000, 0, market(100, 10, 100, 200), noTrader, defaults()); r.Fired {
		t.Fatalf("zero price must not fire: %+v", r)
	}
}

func TestNonPositiveNotionalIgnored(t *testing.T) {
	if r := Score(-1, 0.5, market(100, 10, 100, 200), noTrader, defaults()); r.Fired {
		t.Fatal("negative notional must not fire")
	}
}

func TestPriceAtOrAboveOneRejected(t *testing.T) {
	// price ≥ 1 means odds ≤ 1 — there is no payoff.
	if r := Score(10_000, 1.0, market(200, 50, 100, 300), noTrader, defaults()); r.Fired {
		t.Fatalf("price≥1 must not fire: %+v", r)
	}
}

// --- Low-baseline confidence cap (v6) -------------------------------------

// TestLowBaselineCap_FiringDowngradedToInfo pins the v6 contract: when
// the market baseline is unready and LowBaselineCapEnabled=true, the
// firing severity is capped at LowBaselineSingleMaxSeverity even if
// the absolute+payoff floors would have qualified a higher tier.
func TestLowBaselineCap_FiringDowngradedToInfo(t *testing.T) {
	th := defaults()
	th.LowBaselineCapEnabled = true
	th.LowBaselineSingleMaxSeverity = anomaly.SeverityInfo
	th.LowBaselineAllowCriticalAbsolute = true

	// $30k @ odds 6 — would fire at Warning under defaults if market
	// were ready. Market unready (noMarket) → cap to Info.
	r := Score(30_000, 1.0/6, noMarket, trader(60, 300, 3_000, 8_000), th)
	if !r.Fired {
		t.Fatalf("expected fire: %+v", r)
	}
	if r.Severity != anomaly.SeverityInfo {
		t.Fatalf("severity must be capped to Info, got %s", r.Severity)
	}
	if !r.SeverityCapped || !r.LowMarketBaselineConfidence {
		t.Errorf("expected SeverityCapped + LowMarketBaselineConfidence: %+v", r)
	}
}

// TestLowBaselineCap_CriticalAbsoluteExceptionAllowed pins the
// LowBaselineAllowCriticalAbsolute=true escape hatch: a trade that
// clears the Critical absolute floor is NOT capped even when the
// market baseline is unready.
func TestLowBaselineCap_CriticalAbsoluteExceptionAllowed(t *testing.T) {
	th := defaults()
	th.LowBaselineCapEnabled = true
	th.LowBaselineSingleMaxSeverity = anomaly.SeverityInfo
	th.LowBaselineAllowCriticalAbsolute = true

	r := Score(500_000, 1.0/10, noMarket, trader(60, 300, 3_000, 8_000), th)
	if !r.Fired {
		t.Fatalf("expected fire: %+v", r)
	}
	if r.Severity != anomaly.SeverityCritical {
		t.Fatalf("Critical absolute should bypass cap, got %s", r.Severity)
	}
	if r.SeverityCapped {
		t.Errorf("SeverityCapped should be false when exception applies: %+v", r)
	}
}

// TestLowBaselineCap_DisabledIsNoop pins that LowBaselineCapEnabled=false
// preserves the original severity (the confidence flags still fire,
// but severity is not adjusted).
func TestLowBaselineCap_DisabledIsNoop(t *testing.T) {
	th := defaults()
	th.LowBaselineCapEnabled = false

	r := Score(30_000, 1.0/6, noMarket, trader(60, 300, 3_000, 8_000), th)
	if !r.Fired {
		t.Fatalf("expected fire: %+v", r)
	}
	if r.Severity != anomaly.SeverityWarning {
		t.Errorf("severity should not be capped when feature off, got %s", r.Severity)
	}
	if !r.LowMarketBaselineConfidence {
		t.Error("LowMarketBaselineConfidence flag should still be set")
	}
	if r.SeverityCapped {
		t.Errorf("SeverityCapped should be false when feature off")
	}
}

// TestP99OnlyGateEnforced verifies that a tier with only p99 configured
// still gates correctly when the market baseline is ready.
func TestP99OnlyGateEnforced(t *testing.T) {
	th := defaults()
	th.Info.MinMarketP95Ratio = 0
	th.Info.MinMarketP99Ratio = 2 // must be 2× p99
	th.Info.MinProfitUSD = 0      // disable payoff so p99 is the only blocker
	// $14000 trade clears Info notional ($10k) and odds (3); market p99
	// $10000 → ratio 1.4 < 2 → no fire.
	r := Score(14_000, 1.0/3, market(200, 50, 100, 10_000), noTrader, th)
	if r.Fired {
		t.Fatalf("expected no fire below p99-only gate: %+v", r)
	}
	if r.SuppressedReason != SuppressedBelowMarketP99 {
		t.Errorf("expected SuppressedBelowMarketP99, got %q", r.SuppressedReason)
	}
}
