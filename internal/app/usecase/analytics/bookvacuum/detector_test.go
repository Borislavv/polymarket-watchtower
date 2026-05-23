package bookvacuum

import "testing"

func TestDecide_AskCollapseWithMidUpFiresBoost(t *testing.T) {
	in := Input{
		Recent: FeatureBar{
			AskDepthDeltaPct: -0.7, // 70% ask-side collapse
			BidDepthDeltaPct: 0,
			MidDelta:         0.015,
		},
		SpreadZ: 2.5,
	}
	v := New(Config{}).Decide(in)
	if !v.Detected || v.Side != "ask" {
		t.Fatalf("expected ask-side vacuum; got %+v", v)
	}
	if v.Boost <= 0 {
		t.Errorf("boost must be positive; got %v", v.Boost)
	}
}

func TestDecide_BothSidesRestoredNoFire(t *testing.T) {
	in := Input{
		Recent: FeatureBar{
			AskDepthDeltaPct: -0.6,
			BidDepthDeltaPct: -0.6,
			MidDelta:         0,
		},
		SpreadZ: 0,
	}
	v := New(Config{}).Decide(in)
	if v.Detected {
		t.Fatalf("must not fire on symmetric collapse with no spread or mid confirmation; got %+v", v)
	}
}

func TestDecide_MMLikeSkipped(t *testing.T) {
	in := Input{
		Recent:  FeatureBar{AskDepthDeltaPct: -0.9, MidDelta: 0.02},
		SpreadZ: 5,
		MMLike:  true,
	}
	v := New(Config{}).Decide(in)
	if v.Detected {
		t.Fatalf("MM-like markets must be skipped; got %+v", v)
	}
}
