package repricinglag

import "testing"

// Target +1c, peers +6c → underreaction fires.
func TestDecide_UnderreactionFires(t *testing.T) {
	in := Input{
		ObservedMoveCents: 1,
		PeerMovesCents:    []float64{6, 7, 5, 6, 8},
	}
	v := New(Config{}).Decide(in)
	if !v.Fired || v.SideBias != "underreaction" {
		t.Fatalf("expected underreaction; got %+v", v)
	}
}

// Target moves in line with peers → no fire.
func TestDecide_InLineWithPeers(t *testing.T) {
	in := Input{
		ObservedMoveCents: 5,
		PeerMovesCents:    []float64{5, 4, 6, 5, 5},
	}
	v := New(Config{}).Decide(in)
	if v.Fired {
		t.Fatalf("must not fire when in line with peers; got %+v", v)
	}
}

// High ambiguity blocks regardless of lag.
func TestDecide_HighAmbiguityBlocks(t *testing.T) {
	in := Input{
		ObservedMoveCents: 0,
		PeerMovesCents:    []float64{8, 9, 10},
		AmbiguityScore:    0.9,
	}
	v := New(Config{}).Decide(in)
	if v.Fired {
		t.Fatalf("ambiguity must block; got %+v", v)
	}
}

// Overreaction symmetric path.
func TestDecide_OverreactionFires(t *testing.T) {
	in := Input{
		ObservedMoveCents: 10,
		PeerMovesCents:    []float64{2, 1, 3, 2, 2},
	}
	v := New(Config{}).Decide(in)
	if !v.Fired || v.SideBias != "overreaction" {
		t.Fatalf("expected overreaction; got %+v", v)
	}
}
