package score

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

func defaultThresholds() anomaly.Thresholds {
	t := anomaly.Thresholds{
		Multipliers:       []float64{30, 100, 1000},
		AbsoluteUSDTiers:  []float64{3_000, 10_000, 100_000},
		MinBaselineTrades: 20,
	}
	t.Normalise()
	return t
}

func bs(count int, median float64) baseline.Stats {
	return baseline.Stats{Count: count, MedianUSD: median, MeanUSD: median}
}

func TestMultiplierLadderTiers(t *testing.T) {
	cfg := defaultThresholds()
	cases := []struct {
		name     string
		notional float64
		baseline baseline.Stats
		wantFire bool
		wantSev  anomaly.Severity
		wantMul  float64
	}{
		{"below_lowest", 290, bs(50, 10), false, "", 0},
		{"x30_info", 300, bs(50, 10), true, anomaly.SeverityInfo, 30},
		{"x99_info", 990, bs(50, 10), true, anomaly.SeverityInfo, 99},
		{"x100_warning", 1000, bs(50, 10), true, anomaly.SeverityWarning, 100},
		{"x999_warning", 9990, bs(50, 10), true, anomaly.SeverityWarning, 999},
		{"x1000_critical_but_low_baseline_skipped", 10_000, bs(10, 10), true, anomaly.SeverityWarning, 0}, // multiplier skipped, absolute tier still fires
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Score(c.notional, c.baseline, cfg)
			if r.Fired != c.wantFire {
				t.Fatalf("fire: got %v want %v (%+v)", r.Fired, c.wantFire, r)
			}
			if c.wantFire && r.Severity != c.wantSev {
				t.Fatalf("severity: got %s want %s", r.Severity, c.wantSev)
			}
			if c.wantMul != 0 && r.Multiplier != c.wantMul {
				t.Fatalf("multiplier: got %v want %v", r.Multiplier, c.wantMul)
			}
		})
	}
}

func TestLowBaselineDoesNotFireMultiplier(t *testing.T) {
	cfg := defaultThresholds()
	// Notional 500, baseline median 10, ratio 50 → would be info. But Count < MinBaselineTrades.
	r := Score(500, bs(5, 10), cfg)
	if r.Fired {
		t.Fatalf("expected no fire (low sample): %+v", r)
	}
}

func TestZeroBaselineNoDivisionPanic(t *testing.T) {
	cfg := defaultThresholds()
	// Median 0 ⇒ multiplier ladder skipped; absolute tier still applies.
	r := Score(50_000, bs(100, 0), cfg)
	if !r.Fired {
		t.Fatal("expected absolute tier to fire")
	}
	if r.Reason != "absolute_tier" {
		t.Fatalf("reason: %s", r.Reason)
	}
}

func TestAbsoluteTierFiresIndependently(t *testing.T) {
	cfg := defaultThresholds()
	cases := []struct {
		notional float64
		wantSev  anomaly.Severity
		wantTier float64
	}{
		{2_999, "", 0},
		{3_000, anomaly.SeverityInfo, 3_000},
		{9_999, anomaly.SeverityInfo, 3_000},
		{10_000, anomaly.SeverityWarning, 10_000},
		{99_999, anomaly.SeverityWarning, 10_000},
		{100_000, anomaly.SeverityCritical, 100_000},
		{1_000_000, anomaly.SeverityCritical, 100_000},
	}
	for _, c := range cases {
		r := Score(c.notional, bs(0, 0), cfg) // empty baseline
		if c.wantSev == "" {
			if r.Fired {
				t.Errorf("$%.0f: unexpected fire %+v", c.notional, r)
			}
			continue
		}
		if !r.Fired || r.Severity != c.wantSev || r.AbsoluteTier != c.wantTier {
			t.Errorf("$%.0f: got %+v want sev=%s tier=%v", c.notional, r, c.wantSev, c.wantTier)
		}
	}
}

func TestBothSignalsTakeMaxSeverity(t *testing.T) {
	cfg := defaultThresholds()
	// Notional 100k, baseline median 10 → multiplier=10_000 → critical.
	// Absolute tier 100k → critical. Both fire; reason is the combined string.
	r := Score(100_000, bs(50, 10), cfg)
	if !r.Fired {
		t.Fatal("expected fire")
	}
	if r.Severity != anomaly.SeverityCritical {
		t.Fatalf("severity: %s", r.Severity)
	}
	if r.Reason != "multiplier+absolute_tier" {
		t.Fatalf("reason: %s", r.Reason)
	}
	if r.Multiplier != 10_000 || r.AbsoluteTier != 100_000 {
		t.Fatalf("multiplier=%v absTier=%v", r.Multiplier, r.AbsoluteTier)
	}
}

func TestNonPositiveNotionalIgnored(t *testing.T) {
	cfg := defaultThresholds()
	if r := Score(-1, bs(100, 10), cfg); r.Fired {
		t.Fatalf("negative notional fired: %+v", r)
	}
	if r := Score(0, bs(100, 10), cfg); r.Fired {
		t.Fatalf("zero notional fired: %+v", r)
	}
}
