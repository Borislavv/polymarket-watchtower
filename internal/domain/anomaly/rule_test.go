package anomaly

import "testing"

func TestRuleSeverityLadder(t *testing.T) {
	r := Rule{Multipliers: []float64{1000, 30, 100}}
	r.Normalise()
	if got := r.Multipliers; got[0] != 30 || got[1] != 100 || got[2] != 1000 {
		t.Fatalf("normalise: %v", got)
	}

	cases := []struct {
		ratio float64
		want  Severity
		fire  bool
	}{
		{ratio: 29, want: "", fire: false},
		{ratio: 30, want: SeverityWarn, fire: true},
		{ratio: 99.9, want: SeverityWarn, fire: true},
		{ratio: 100, want: SeverityCritical, fire: true},
		{ratio: 999, want: SeverityCritical, fire: true},
		{ratio: 1000, want: SeverityFatal, fire: true},
		{ratio: 1e6, want: SeverityFatal, fire: true},
	}
	for _, c := range cases {
		got, ok := r.SeverityFor(c.ratio)
		if ok != c.fire || got != c.want {
			t.Fatalf("ratio=%v: got (%s, %v) want (%s, %v)", c.ratio, got, ok, c.want, c.fire)
		}
	}
}
