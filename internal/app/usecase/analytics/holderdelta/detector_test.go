package holderdelta

import (
	"testing"
	"time"
)

func ts(h int) time.Time { return time.Date(2026, 5, 22, h, 0, 0, 0, time.UTC) }

// Wallet legitimately grew from rank 9 to rank 3 with pctOI 4% → 12%, OI flat.
func TestDecide_TrueAccumulationFiresWarning(t *testing.T) {
	in := Input{
		ConditionID:  "0xA",
		OutcomeToken: "YES",
		Wallet:       "0xW",
		Now:          ts(12),
		Previous: Snapshot{
			SnapshotAt: ts(0), Wallet: "0xW", Rank: 9, Shares: 4_000,
			NotionalUSD: 1_000, PctOI: 0.04, TotalOI: 100_000,
		},
		Current: Snapshot{
			SnapshotAt: ts(12), Wallet: "0xW", Rank: 3, Shares: 12_000,
			NotionalUSD: 5_000, PctOI: 0.12, TotalOI: 100_000,
		},
	}
	d := New(Config{})
	v := d.Decide(in)
	if !v.Fired || v.Level != "warning" {
		t.Fatalf("want fired=warning; got fired=%v level=%s score=%v",
			v.Fired, v.Level, v.Score)
	}
	if v.DenominatorPenalty != 1.0 {
		t.Errorf("OI didn't shrink — penalty must be 1.0; got %v", v.DenominatorPenalty)
	}
}

// Wallet's pctOI "grew" only because OI collapsed; absolute shares flat.
func TestDecide_DenominatorArtifactSuppresses(t *testing.T) {
	in := Input{
		ConditionID: "0xA", OutcomeToken: "YES", Wallet: "0xW", Now: ts(12),
		Previous: Snapshot{
			SnapshotAt: ts(0), Rank: 9, Shares: 4_000, PctOI: 0.04, TotalOI: 100_000,
		},
		Current: Snapshot{
			// OI collapsed from 100k → 20k; wallet shares unchanged.
			SnapshotAt: ts(12), Rank: 3, Shares: 4_100, PctOI: 0.205, TotalOI: 20_000,
		},
	}
	d := New(Config{})
	v := d.Decide(in)
	if v.DenominatorPenalty == 1.0 {
		t.Fatalf("penalty must apply when OI collapsed and shares didn't grow")
	}
	// The penalty causes fired=false because the gates require
	// denomPenalty == 1.0.
	if v.Fired {
		t.Fatalf("must NOT fire on denominator artifact; got %+v", v)
	}
}

// Wallet enters top-5 with material share growth → info fires even
// at modest pctOI.
func TestDecide_EnteredTopKFiresInfo(t *testing.T) {
	in := Input{
		ConditionID: "0xA", OutcomeToken: "YES", Wallet: "0xW", Now: ts(12),
		Previous: Snapshot{Rank: 11, Shares: 1_000, PctOI: 0.01, TotalOI: 100_000},
		Current:  Snapshot{Rank: 4, Shares: 3_500, PctOI: 0.02, TotalOI: 100_000},
	}
	d := New(Config{})
	v := d.Decide(in)
	if !v.Fired {
		t.Fatalf("entered top-K with share growth must fire; got %+v", v)
	}
	if v.Level != "info" {
		t.Errorf("entered-top-K path should fire info; got %s", v.Level)
	}
}

// pctOI well below the info tier and no rank improvement → no-fire.
func TestDecide_BelowAllTiers(t *testing.T) {
	in := Input{
		Previous: Snapshot{Rank: 50, Shares: 100, PctOI: 0.001, TotalOI: 50_000},
		Current:  Snapshot{Rank: 49, Shares: 110, PctOI: 0.002, TotalOI: 50_000},
	}
	v := New(Config{}).Decide(in)
	if v.Fired {
		t.Fatalf("must not fire below all tiers; got %+v", v)
	}
}
