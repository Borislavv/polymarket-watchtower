package cheaptail

import (
	"testing"
	"time"
)

func tradeAt(price, notional float64) Trade {
	return Trade{Price: price, NotionalUSD: notional, Side: "YES", Timestamp: time.Now()}
}

// Wallet stages 3 tail trades on a non-ambiguous market with active catalyst.
func TestDecide_StagedTailFires(t *testing.T) {
	in := Input{
		ConditionID:       "0xA",
		Wallet:            "0xW",
		Trades:            []Trade{tradeAt(0.04, 5_000), tradeAt(0.05, 4_000), tradeAt(0.06, 3_500)},
		ThesisBreadth:     2,
		HasActiveCatalyst: true,
		AmbiguityScore:    0.2,
		LifecyclePct:      80,
	}
	v := New(Config{}).Decide(in)
	if !v.Fired {
		t.Fatalf("expected fired; got %+v", v)
	}
	if v.ProbBand != "deep_tail" {
		t.Errorf("expected deep_tail; got %s", v.ProbBand)
	}
}

// Dust notional — no fire.
func TestDecide_DustStagingIgnored(t *testing.T) {
	in := Input{
		Trades:            []Trade{tradeAt(0.04, 100), tradeAt(0.05, 80)},
		HasActiveCatalyst: true,
	}
	v := New(Config{}).Decide(in)
	if v.Fired {
		t.Fatalf("dust must not fire; got %+v", v)
	}
}

// Single trade — staging requires repetition.
func TestDecide_SingleTradeNotStaging(t *testing.T) {
	in := Input{
		Trades:            []Trade{tradeAt(0.04, 10_000)},
		HasActiveCatalyst: true,
	}
	v := New(Config{}).Decide(in)
	if v.Fired {
		t.Fatalf("single trade not staging; got %+v", v)
	}
}

// High ambiguity blocks.
func TestDecide_HighAmbiguityBlocks(t *testing.T) {
	in := Input{
		Trades:            []Trade{tradeAt(0.04, 5_000), tradeAt(0.05, 5_000)},
		HasActiveCatalyst: true,
		AmbiguityScore:    0.9,
	}
	v := New(Config{}).Decide(in)
	if v.Fired {
		t.Fatalf("ambiguity must block; got %+v", v)
	}
}
