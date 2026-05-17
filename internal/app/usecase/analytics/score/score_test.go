package score

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

func defaults() anomaly.Thresholds {
	t := anomaly.Thresholds{
		MultiplierLadder:       []float64{30, 100, 1000},
		OddsLadder:             []float64{3, 10, 25},
		MinTradeUSD:            10_000,
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}
	t.Normalise()
	return t
}

func bs(count int, median, total float64) baseline.Stats {
	return baseline.Stats{Count: count, MedianUSD: median, MeanUSD: median, TotalUSD: total}
}

func TestWhaleMultiplierTiers(t *testing.T) {
	cfg := defaults()
	// baseline median $100, total $50k, 200 samples — generous floor passes.
	b := bs(200, 100, 50_000)
	cases := []struct {
		name     string
		notional float64
		price    float64
		wantSev  anomaly.Severity
		wantMul  float64
	}{
		{"below_min_trade_usd_no_fire", 9_999, 0.5, "", 0},
		{"x100_warning", 10_000, 0.5, anomaly.SeverityWarning, 100},
		{"x1000_critical", 100_000, 0.5, anomaly.SeverityCritical, 1000},
		{"x30_info", 3_000, 0.5, "", 0}, // skipped — below MinTradeUSD
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Score(c.notional, c.price, b, cfg)
			if c.wantSev == "" {
				if r.Fired {
					t.Errorf("unexpected fire: %+v", r)
				}
				return
			}
			if !r.Fired || r.Severity != c.wantSev {
				t.Fatalf("got severity=%s fired=%v want=%s", r.Severity, r.Fired, c.wantSev)
			}
			if r.Reason != anomaly.ReasonWhale {
				t.Fatalf("reason: %s", r.Reason)
			}
			if r.Multiplier != c.wantMul {
				t.Fatalf("multiplier: %v want %v", r.Multiplier, c.wantMul)
			}
		})
	}
}

func TestWhaleSkippedWhenBaselineTooThin(t *testing.T) {
	cfg := defaults()
	// huge trade vs $1 baseline median but only 5 samples — must not fire whale
	r := Score(50_000, 0.5, bs(5, 1, 5), cfg)
	if r.Fired {
		t.Fatalf("expected no fire on thin baseline, got %+v", r)
	}
}

func TestWhaleSkippedWhenBaselineNotionalTooLow(t *testing.T) {
	cfg := defaults()
	// 200 samples but baseline total is $10 — meaningless, do not score.
	r := Score(50_000, 0.5, bs(200, 0.05, 10), cfg)
	if r.Fired {
		t.Fatalf("expected no fire on low-notional baseline, got %+v", r)
	}
}

func TestHighOddsAlone(t *testing.T) {
	cfg := defaults()
	// price 0.05 => odds 20 => warning rung (>= 10).
	r := Score(2_000, 0.05, baseline.Stats{}, cfg)
	if !r.Fired {
		t.Fatalf("expected fire, got %+v", r)
	}
	if r.Reason != anomaly.ReasonHighOdds {
		t.Fatalf("reason: %s", r.Reason)
	}
	if r.Severity != anomaly.SeverityWarning {
		t.Fatalf("severity: %s", r.Severity)
	}
	if r.OddsRung != 10 {
		t.Fatalf("oddsRung: %v", r.OddsRung)
	}
}

func TestHighOddsSilencedBelowFloor(t *testing.T) {
	cfg := defaults()
	// $1 bet at price 0.001 (odds 1000) — below MinTradeUSD/10, must not fire.
	r := Score(1, 0.001, baseline.Stats{}, cfg)
	if r.Fired {
		t.Fatalf("expected no fire on tiny notional, got %+v", r)
	}
}

func TestHighOddsTiers(t *testing.T) {
	cfg := defaults()
	cases := []struct {
		price   float64
		wantSev anomaly.Severity
		want    bool
	}{
		{0.5, "", false},                        // odds 2 — below low rung
		{0.30, anomaly.SeverityInfo, true},      // odds ~3.33
		{0.10, anomaly.SeverityWarning, true},   // odds 10
		{0.04, anomaly.SeverityCritical, true},  // odds 25
		{0.001, anomaly.SeverityCritical, true}, // odds 1000 — still critical (top rung)
	}
	for _, c := range cases {
		r := Score(5_000, c.price, baseline.Stats{}, cfg)
		if r.Fired != c.want {
			t.Errorf("price=%v fire=%v want=%v: %+v", c.price, r.Fired, c.want, r)
			continue
		}
		if c.want && r.Severity != c.wantSev {
			t.Errorf("price=%v severity=%s want=%s", c.price, r.Severity, c.wantSev)
		}
	}
}

func TestWhaleAndOddsCombined(t *testing.T) {
	cfg := defaults()
	// $50k at odds 25 with median $100 baseline (200 samples, $50k total).
	// Multiplier = 500 → warning. Odds = 25 → critical. Combined = critical.
	r := Score(50_000, 0.04, bs(200, 100, 50_000), cfg)
	if !r.Fired {
		t.Fatal("expected fire")
	}
	if r.Reason != anomaly.ReasonHighOddsWhale {
		t.Fatalf("reason: %s", r.Reason)
	}
	if r.Severity != anomaly.SeverityCritical {
		t.Fatalf("severity: %s", r.Severity)
	}
	if r.Multiplier != 500 {
		t.Fatalf("multiplier: %v", r.Multiplier)
	}
	if r.OddsRung != 25 {
		t.Fatalf("oddsRung: %v", r.OddsRung)
	}
}

func TestZeroPriceNoPanic(t *testing.T) {
	cfg := defaults()
	if r := Score(10_000, 0, baseline.Stats{}, cfg); r.Fired {
		t.Fatalf("expected no fire on zero price, got %+v", r)
	}
}

func TestNonPositiveNotionalIgnored(t *testing.T) {
	cfg := defaults()
	if r := Score(-1, 0.5, baseline.Stats{}, cfg); r.Fired {
		t.Fatalf("negative notional fired: %+v", r)
	}
}
