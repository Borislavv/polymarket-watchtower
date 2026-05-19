package accumulation

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
)

func thresholds() anomaly.Thresholds {
	return anomaly.Thresholds{
		Info: anomaly.Tier{
			MinNotionalUSD: 5_000, MinOdds: 3,
			MinProfitUSD:      5_000,
			MinMarketP95Ratio: 1,
			MinTraderP95Ratio: 1,
			MinMultiplier:     100, // accumulation-legacy field — ignored by v6 gates
		},
		Warning: anomaly.Tier{
			MinNotionalUSD: 25_000, MinOdds: 5,
			MinProfitUSD:      15_000,
			MinMarketP95Ratio: 2,
			MinTraderP95Ratio: 1.5,
			MinMultiplier:     1_000,
		},
		Critical: anomaly.Tier{
			MinNotionalUSD: 100_000, MinOdds: 8,
			MinProfitUSD:      50_000,
			MinMarketP95Ratio: 4,
			MinTraderP95Ratio: 2,
			MinMultiplier:     10_000,
		},
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}
}

func cfg() Config {
	return Config{
		Enabled:              true,
		Window:               24 * time.Hour,
		MinTrades:            3,
		TradeFractionOfInfo:  0.60,
		TotalMultiplier:      2,
		ManySmallsMultiplier: 4,
		HardMultiplier:       3,
		Cooldown:             30 * time.Minute,
	}
}

// baseLine is a "well-shaped" line used as the starting point; tests
// mutate specific fields to pin a single dimension at a time.
func baseLine() Line {
	return Line{
		Wallet:       "0xwhale",
		MarketID:     "0xa",
		OutcomeToken: "tok-yes",
		Side:         trade.SideBuy,
		TradeCount:   5,
		// Total $25k across 5 trades — mean $5k, median $5k.
		TotalNotionalUSD:  25_000,
		MeanNotionalUSD:   5_000,
		MedianNotionalUSD: 5_000,
		MaxNotionalUSD:    8_000,
		MinNotionalUSD:    3_000,
		AvgOdds:           5,
		MaxOdds:           10,
		OldestAt:          time.Now().Add(-2 * time.Hour),
		NewestAt:          time.Now(),
		MarketMedianUSD:   100, // 25000/100 = 250× → above Info=100 and Warning=1000? no, 250 < 1000
		MarketP95USD:      500,
		TraderMedianUSD:   1_000,
		TraderP95USD:      3_000,
		LifecyclePct:      80,
	}
}

func TestDecide_DisabledNeverFires(t *testing.T) {
	c := cfg()
	c.Enabled = false
	d := New(c, thresholds())
	v := d.Decide(baseLine())
	if v.Fired {
		t.Fatalf("disabled detector must never fire: %+v", v)
	}
}

func TestDecide_BelowMinTradesIsSilent(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 2 // below MinTrades=3
	v := d.Decide(l)
	if v.Fired {
		t.Fatalf("expected no fire below MinTrades: %+v", v)
	}
}

func TestDecide_SingleTradeDoesNotFire(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 1
	if v := d.Decide(l); v.Fired {
		t.Fatalf("single trade must never qualify accumulation: %+v", v)
	}
}

// TestDecide_InfoMeaningfulPath pins the canonical case from the spec:
// 4 × $3k = $12k @ Info $5k. Median $3k = 0.60 × $5k clears meaningful
// path. Total $12k ≥ 2 × $5k = $10k. Trades 4 ≥ 3. Should fire Info.
func TestDecide_InfoMeaningfulPath(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 4
	l.TotalNotionalUSD = 12_000
	l.MeanNotionalUSD = 3_000
	l.MedianNotionalUSD = 3_000
	l.MaxNotionalUSD = 4_000
	l.MinNotionalUSD = 2_500
	l.MarketMedianUSD = 50 // 12000/50 = 240× ≥ Info=100
	l.AvgOdds = 4          // ≥ Info=3
	v := d.Decide(l)
	if !v.Fired || v.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected Info, got %+v", v)
	}
	if v.SizePath != "meaningful" {
		t.Errorf("expected meaningful size path, got %q", v.SizePath)
	}
	if !containsReason(v.Reasons, ReasonRepeatedSameOutcome) {
		t.Errorf("missing canonical reason: %v", v.Reasons)
	}
}

// TestDecide_InfoManySmallsPath pins the headline 200 × $200 = $40k
// case. Median $200 = 4% of Info $5k → meaningful path fails. Total
// $40k = 8 × Info $5k ≥ 4 × Info ⇒ many-smalls passes. Should fire Info.
func TestDecide_InfoManySmallsPath(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 200
	l.TotalNotionalUSD = 40_000
	l.MeanNotionalUSD = 200
	l.MedianNotionalUSD = 200
	l.MaxNotionalUSD = 250
	l.MinNotionalUSD = 150
	l.MarketMedianUSD = 50 // 40000/50 = 800× ≥ Info=100
	l.AvgOdds = 4
	v := d.Decide(l)
	if !v.Fired || v.Severity != anomaly.SeverityInfo {
		t.Fatalf("expected Info on many-smalls, got %+v", v)
	}
	if v.SizePath != "many-smalls" {
		t.Errorf("expected many-smalls size path, got %q", v.SizePath)
	}
	if !containsReason(v.Reasons, ReasonManySmallSameSide) {
		t.Errorf("missing many-smalls reason: %v", v.Reasons)
	}
}

// TestDecide_NotEnoughForInfo pins the lower edge — 3 × $3k = $9k. Total
// $9k < 2 × Info $5k = $10k (meaningful fails). $9k < 4 × Info $5k = $20k
// (many-smalls fails). Should NOT fire.
func TestDecide_NotEnoughForInfo(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 3
	l.TotalNotionalUSD = 9_000
	l.MeanNotionalUSD = 3_000
	l.MedianNotionalUSD = 3_000
	l.MarketMedianUSD = 50 // multiplier OK
	l.AvgOdds = 4
	if v := d.Decide(l); v.Fired {
		t.Fatalf("expected no fire — total under both size paths: %+v", v)
	}
}

// TestDecide_WarningTier pins 10 × $5k = $50k @ Warning $25k. Median $5k
// = 0.20 × Warning $25k = $5k → 0.60 fraction would require ≥ $15k →
// meaningful fails for Warning. Total $50k ≥ 4 × Warning $25k = $100k →
// many-smalls fails for Warning. → Drops to Info. Total $50k ≥ 2 ×
// Info $5k = $10k AND median $5k ≥ 0.60 × Info $5k = $3k → Info meaningful.
//
// (Pinning the spec's example: "10 × $5k = $50k → warning/critical
// depending thresholds" with our specific Info=$5k anchor, this lands at
// Info, which is the right answer.)
func TestDecide_TenByFiveK(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 10
	l.TotalNotionalUSD = 50_000
	l.MeanNotionalUSD = 5_000
	l.MedianNotionalUSD = 5_000
	l.MaxNotionalUSD = 7_000
	l.MinNotionalUSD = 3_000
	l.MarketMedianUSD = 50 // 50k/50 = 1000× ≥ Warning multiplier
	l.AvgOdds = 5          // ≥ Warning odds
	v := d.Decide(l)
	if !v.Fired {
		t.Fatalf("expected fire on 10×$5k, got %+v", v)
	}
	// With this anchor we land at Info because Warning's size + median
	// gates are stricter (Warning anchor is $25k, not $5k).
	if v.Severity != anomaly.SeverityInfo {
		t.Errorf("expected Info severity at these thresholds, got %s", v.Severity)
	}
}

// TestDecide_OddsGateBlocksLowOdds pins the asymmetric-payoff requirement.
// 4 × $5k = $20k @ price 0.50 (odds 2). Total + multiplier + count clear
// Info, but avg odds = 2 < Info odds 3 → no fire.
func TestDecide_OddsGateBlocksLowOdds(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 4
	l.TotalNotionalUSD = 20_000
	l.MeanNotionalUSD = 5_000
	l.MedianNotionalUSD = 5_000
	l.MarketMedianUSD = 50 // multiplier OK
	l.AvgOdds = 2          // BELOW Info odds 3
	if v := d.Decide(l); v.Fired {
		t.Fatalf("expected no fire on low odds: %+v", v)
	}
}

// TestDecide_MarketP95GateBlocksLowRatio pins the v6 tail gate: a line
// whose total is below the market p95 ratio fails the tail gate, even
// when the size paths qualify.
func TestDecide_MarketP95GateBlocksLowRatio(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 5
	l.TotalNotionalUSD = 25_000
	l.MeanNotionalUSD = 5_000
	l.MedianNotionalUSD = 5_000
	l.MaxNotionalUSD = 6_000
	// market p95 $30k → ratio 0.83 < Info 1.0× → blocked.
	l.MarketP95USD = 30_000
	l.TraderP95USD = 10_000 // trader p95 ratio 2.5 ≥ Info 1.0×, so trader axis passes
	l.AvgOdds = 5
	if v := d.Decide(l); v.Fired {
		t.Fatalf("expected no fire below market p95 ratio: %+v", v)
	}
}

// TestDecide_TraderP95GateBlocksRoutineWhale pins the v6 trader-tail
// gate: a wallet whose own p95 dwarfs the line total fails the trader
// gate (line is routine for this wallet), regardless of market tail.
func TestDecide_TraderP95GateBlocksRoutineWhale(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 5
	l.TotalNotionalUSD = 25_000
	l.MeanNotionalUSD = 5_000
	l.MedianNotionalUSD = 5_000
	l.MarketP95USD = 100 // market tail comfortable (ratio 250×)
	// trader p95 $100k → ratio 0.25 < Info 1.0× → blocked.
	l.TraderP95USD = 100_000
	l.AvgOdds = 5
	if v := d.Decide(l); v.Fired {
		t.Fatalf("expected no fire when line is routine for this wallet: %+v", v)
	}
}

// TestDecide_BaselinesUnreadyFireWithLowConfidence pins the v6
// philosophy: when neither baseline is ready the line CAN still fire
// (size + payoff + odds + min trades), but it carries the
// LowBaselineConfidence reason code so an operator can see the
// uncertainty.
func TestDecide_BaselinesUnreadyFireWithLowConfidence(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 5
	l.TotalNotionalUSD = 25_000
	l.MeanNotionalUSD = 5_000
	l.MedianNotionalUSD = 5_000
	l.MarketP95USD = 0 // unready
	l.TraderP95USD = 0 // unready
	l.MarketMedianUSD = 0
	l.TraderMedianUSD = 0
	l.AvgOdds = 5
	v := d.Decide(l)
	if !v.Fired {
		t.Fatalf("expected fire on absolute+payoff alone when no baseline ready: %+v", v)
	}
	sawLowConfidence := false
	for _, r := range v.Reasons {
		if r == ReasonLowBaselineConfidence {
			sawLowConfidence = true
		}
	}
	if !sawLowConfidence {
		t.Errorf("LowBaselineConfidence reason must be attached when baselines unready: %v", v.Reasons)
	}
}

// TestDecide_PayoffGateBlocksLowOdds pins the v6 payoff gate: the
// line might clear notional + tail, but if profit-if-win is below
// tier.MinProfitUSD it must not fire. Choose odds just above the
// MinOdds floor so the odds gate doesn't bite first.
func TestDecide_PayoffGateBlocksLowOdds(t *testing.T) {
	_ = New(cfg(), thresholds()) // confirm constructor still works
	l := baseLine()
	l.TradeCount = 5
	l.TotalNotionalUSD = 12_000 // ≥ Info 2× notional $5k=$10k
	l.MeanNotionalUSD = 2_400
	l.MedianNotionalUSD = 2_400
	l.MaxNotionalUSD = 3_000
	l.MarketP95USD = 200 // ratio 60 ≥ 1
	l.TraderP95USD = 200 // ratio 60 ≥ 1
	l.AvgOdds = 3.1      // profit = 12000 × 2.1 = $25,200… too high. Lower to 1.3.
	l.AvgOdds = 3.05     // profit = 12000 × 2.05 = $24,600 — still above Info $5k
	// Actually $5k profit floor is generous. Set Info MinProfitUSD higher.
	th := thresholds()
	th.Info.MinProfitUSD = 50_000 // force payoff gate above $24,600
	d2 := New(cfg(), th)
	if v := d2.Decide(l); v.Fired {
		t.Fatalf("expected no fire below payoff floor: profit=%v ; verdict=%+v", l.LineProfitIfWinUSD(), v)
	}
}

// TestDecide_HardPromotionRequiresHotLifecycle pins that Hard is reserved
// for very large lines that ALSO land in the HOT lifecycle window. Same
// line without Hot should stay Critical.
func TestDecide_HardPromotionRequiresHot(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 8
	l.TotalNotionalUSD = 400_000 // ≥ 3 × Critical $100k
	l.MeanNotionalUSD = 50_000
	l.MedianNotionalUSD = 50_000
	l.MarketMedianUSD = 5 // 400000/5 = 80,000× ≥ Critical=10,000
	l.AvgOdds = 10        // ≥ Critical odds 8
	l.Hot = false

	v := d.Decide(l)
	if !v.Fired || v.Severity != anomaly.SeverityCritical {
		t.Fatalf("expected Critical without Hot, got %+v", v)
	}

	l.Hot = true
	v = d.Decide(l)
	if !v.Fired || v.Severity != anomaly.SeverityHard {
		t.Fatalf("expected Hard with Hot lifecycle, got %+v", v)
	}
}

// TestScoreMonotone verifies that score increases as total grows. Both
// inputs must fire at the same severity tier so the comparison is over
// the same anchor.
func TestScoreMonotone(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 5
	// Low case: $20k total / $4k median — just clears Info meaningful.
	l.TotalNotionalUSD = 20_000
	l.MeanNotionalUSD = 4_000
	l.MedianNotionalUSD = 4_000
	l.MarketMedianUSD = 50
	l.AvgOdds = 4
	low := d.Decide(l)
	// High case: same shape with 3× the total.
	l.TotalNotionalUSD = 60_000
	l.MeanNotionalUSD = 12_000
	l.MedianNotionalUSD = 12_000
	high := d.Decide(l)
	if !low.Fired || !high.Fired {
		t.Fatalf("both must fire: %+v / %+v", low, high)
	}
	if low.Severity != high.Severity {
		t.Fatalf("monotone test requires same severity tier: low=%s high=%s", low.Severity, high.Severity)
	}
	if high.Score <= low.Score {
		t.Errorf("score not monotone: low=%d high=%d", low.Score, high.Score)
	}
}

// TestConfidenceShape verifies confidence stays in [0, 1] and grows with
// sample size + span.
func TestConfidenceShape(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.TradeCount = 3
	l.OldestAt = time.Now().Add(-15 * time.Minute)
	l.NewestAt = time.Now()
	thin := d.Decide(l).Confidence

	l.TradeCount = 20
	l.OldestAt = time.Now().Add(-8 * time.Hour)
	thick := d.Decide(l).Confidence

	if thin < 0 || thin > 1 || thick < 0 || thick > 1 {
		t.Errorf("confidence out of range: thin=%v thick=%v", thin, thick)
	}
	if thick <= thin {
		t.Errorf("confidence must grow with sample + span: thin=%v thick=%v", thin, thick)
	}
}

// TestReasonsLateMarketTag pins that lifecycle ≥ 75 surfaces the late-
// market reason code in the alert payload.
func TestReasonsLateMarketTag(t *testing.T) {
	d := New(cfg(), thresholds())
	l := baseLine()
	l.LifecyclePct = 82
	v := d.Decide(l)
	if !v.Fired {
		t.Fatalf("expected fire: %+v", v)
	}
	if !containsReason(v.Reasons, ReasonLateMarketAccumulation) {
		t.Errorf("expected LATE_MARKET reason in %v", v.Reasons)
	}
}

func containsReason(rs []ReasonCode, want ReasonCode) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}
