package anomaly

import "testing"

func TestThresholdsNormaliseSortsAndDedupes(t *testing.T) {
	x := Thresholds{MultiplierLadder: []float64{1000, 30, 30, 100}, OddsLadder: []float64{25, 3, 10}}
	x.Normalise()
	if got := x.MultiplierLadder; len(got) != 3 || got[0] != 30 || got[1] != 100 || got[2] != 1000 {
		t.Fatalf("multipliers: %v", got)
	}
	if got := x.OddsLadder; len(got) != 3 || got[0] != 3 || got[1] != 10 || got[2] != 25 {
		t.Fatalf("odds: %v", got)
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
