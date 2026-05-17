package anomaly

import "testing"

func TestThresholdsNormaliseSortsAndDedupes(t *testing.T) {
	x := Thresholds{Multipliers: []float64{1000, 30, 30, 100}, AbsoluteUSDTiers: []float64{100_000, 3_000, 10_000}}
	x.Normalise()
	wantM := []float64{30, 100, 1000}
	wantA := []float64{3_000, 10_000, 100_000}
	if len(x.Multipliers) != 3 || x.Multipliers[0] != wantM[0] || x.Multipliers[1] != wantM[1] || x.Multipliers[2] != wantM[2] {
		t.Fatalf("multipliers: %v", x.Multipliers)
	}
	if len(x.AbsoluteUSDTiers) != 3 || x.AbsoluteUSDTiers[0] != wantA[0] || x.AbsoluteUSDTiers[2] != wantA[2] {
		t.Fatalf("absolute: %v", x.AbsoluteUSDTiers)
	}
}

func TestSeverityForLadder(t *testing.T) {
	ladder := []float64{30, 100, 1000}
	cases := []struct {
		v       float64
		want    Severity
		fire    bool
		wantHit float64
	}{
		{29, "", false, 0},
		{30, SeverityInfo, true, 30},
		{99.999, SeverityInfo, true, 30},
		{100, SeverityWarning, true, 100},
		{999, SeverityWarning, true, 100},
		{1000, SeverityCritical, true, 1000},
		{1_000_000, SeverityCritical, true, 1000},
	}
	for _, c := range cases {
		sev, hit, ok := SeverityForLadder(c.v, ladder)
		if ok != c.fire || sev != c.want {
			t.Errorf("v=%v: got (%s,%v) want (%s,%v)", c.v, sev, ok, c.want, c.fire)
		}
		if c.fire && hit != c.wantHit {
			t.Errorf("v=%v: hit=%v want %v", c.v, hit, c.wantHit)
		}
	}
}

func TestSeverityForEmptyLadderNeverFires(t *testing.T) {
	if _, _, ok := SeverityForLadder(1e9, nil); ok {
		t.Fatal("empty ladder should never fire")
	}
}

func TestMaxSeverity(t *testing.T) {
	if got := MaxSeverity(SeverityInfo, SeverityCritical); got != SeverityCritical {
		t.Fatalf("got %s", got)
	}
	if got := MaxSeverity(SeverityHard, SeverityCritical); got != SeverityHard {
		t.Fatalf("got %s", got)
	}
	if got := MaxSeverity("", SeverityInfo); got != SeverityInfo {
		t.Fatalf("got %s", got)
	}
}
