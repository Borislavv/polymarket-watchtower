package anomaly

import "testing"

func defaultThresholds() Thresholds {
	return Thresholds{
		Info:                   Tier{MinNotionalUSD: 10_000, MinOdds: 3},
		Warning:                Tier{MinNotionalUSD: 25_000, MinOdds: 5},
		Critical:               Tier{MinNotionalUSD: 100_000, MinOdds: 8},
		MinBaselineTrades:      20,
		MinBaselineNotionalUSD: 1_000,
	}
}

func TestAbsoluteTierRequiresBothNotionalAndOdds(t *testing.T) {
	th := defaultThresholds()
	cases := []struct {
		name           string
		notional, odds float64
		want           Severity
	}{
		{"below_info_notional", 9_999, 5, ""},
		{"below_info_odds", 10_000, 2.99, ""},
		{"info_exact", 10_000, 3, SeverityInfo},
		{"info_high_odds_low_notional", 15_000, 4, SeverityInfo},
		{"warning_both_meet", 25_000, 5, SeverityWarning},
		{"warning_notional_not_critical", 99_999, 8, SeverityWarning},
		{"critical_both_meet", 100_000, 8, SeverityCritical},
		{"critical_odds_not_met", 200_000, 7, SeverityWarning},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := th.AbsoluteTier(c.notional, c.odds); got != c.want {
				t.Fatalf("notional=%v odds=%v: got %q want %q", c.notional, c.odds, got, c.want)
			}
		})
	}
}

func TestConservativeMin(t *testing.T) {
	cases := []struct{ a, b, want Severity }{
		{SeverityInfo, SeverityCritical, SeverityInfo},
		{SeverityCritical, SeverityWarning, SeverityWarning},
		{SeverityWarning, SeverityWarning, SeverityWarning},
		{"", SeverityCritical, ""},
		{SeverityCritical, "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := ConservativeMin(c.a, c.b); got != c.want {
			t.Errorf("ConservativeMin(%q,%q) = %q want %q", c.a, c.b, got, c.want)
		}
	}
}
