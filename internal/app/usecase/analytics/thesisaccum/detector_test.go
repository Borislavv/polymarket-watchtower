// Pure detector tests. No DB, no network.
package thesisaccum

import (
	"testing"
	"time"
)

func baseInput() Input {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	return Input{
		SourceConditionID: "0xA",
		SourceEventSlug:   "us-pres-2028",
		Wallet:            "0xWALLET",
		Side:              "YES",
		Now:               now,
	}
}

// 3 linked markets all aligned, BUY/BUY/BUY → fire warning+.
func TestDecide_BreadthThreeAlignedFiresWarning(t *testing.T) {
	in := baseInput()
	in.Links = []Link{
		{DstConditionID: "0xB", LinkType: "series", Direction: DirAligned, Confidence: 1},
		{DstConditionID: "0xC", LinkType: "series", Direction: DirAligned, Confidence: 1},
	}
	in.WalletLines = []WalletLine{
		{ConditionID: "0xA", Side: "YES", NetSharesUSD: 25_000, Trades: 2, WindowStart: in.Now.Add(-24 * time.Hour), LiquidityFloor: 5_000},
		{ConditionID: "0xB", Side: "YES", NetSharesUSD: 30_000, Trades: 3, WindowStart: in.Now.Add(-48 * time.Hour), LiquidityFloor: 5_000},
		{ConditionID: "0xC", Side: "YES", NetSharesUSD: 20_000, Trades: 2, WindowStart: in.Now.Add(-12 * time.Hour), LiquidityFloor: 5_000},
	}
	d := New(Config{})
	v := d.Decide(in)
	if !v.Fired {
		t.Fatalf("expected fired; got verdict=%+v", v)
	}
	if v.Breadth != 3 {
		t.Errorf("breadth: want 3, got %d", v.Breadth)
	}
	if v.Consistency < 0.99 {
		t.Errorf("consistency must be 1.0 when all aligned; got %v", v.Consistency)
	}
	if v.Level == "none" {
		t.Errorf("level must not be 'none' on a fired verdict")
	}
}

// Opposed exposure equal to aligned brings consistency < 0.75; no fire.
func TestDecide_OpposedExposureBreaksConsistency(t *testing.T) {
	in := baseInput()
	in.Links = []Link{
		{DstConditionID: "0xB", LinkType: "series", Direction: DirAligned, Confidence: 1},
		{DstConditionID: "0xC", LinkType: "series", Direction: DirOpposed, Confidence: 1},
	}
	in.WalletLines = []WalletLine{
		{ConditionID: "0xA", Side: "YES", NetSharesUSD: 10_000, LiquidityFloor: 5_000},
		{ConditionID: "0xB", Side: "YES", NetSharesUSD: 10_000, LiquidityFloor: 5_000},
		// SAME side as alert on an OPPOSED-direction market = contradiction.
		{ConditionID: "0xC", Side: "YES", NetSharesUSD: 20_000, LiquidityFloor: 5_000},
	}
	d := New(Config{})
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must NOT fire when consistency drops below 0.75; got %+v", v)
	}
	if v.OpposedExposure <= 0 {
		t.Errorf("opposed exposure should be tallied; got %v", v.OpposedExposure)
	}
}

// Single-market line — breadth=1, must not fire (spec gate).
func TestDecide_SingleMarketNeverFires(t *testing.T) {
	in := baseInput()
	in.Links = nil // empty graph
	in.WalletLines = []WalletLine{
		{ConditionID: "0xA", Side: "YES", NetSharesUSD: 100_000, LiquidityFloor: 1_000},
	}
	d := New(Config{})
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must NOT fire on breadth=1 even with large notional; got %+v", v)
	}
}

// Empty wallet history doesn't panic and returns no-fire.
func TestDecide_EmptyWalletLinesIsNoOp(t *testing.T) {
	in := baseInput()
	in.Links = []Link{
		{DstConditionID: "0xB", Direction: DirAligned, Confidence: 1},
	}
	in.WalletLines = nil
	d := New(Config{})
	v := d.Decide(in)
	if v.Fired {
		t.Fatalf("must not fire on empty lines; got %+v", v)
	}
}

// Catalyst within window adds positive boost; verify the feature.
func TestDecide_CatalystBoostAddsScore(t *testing.T) {
	in := baseInput()
	in.Links = []Link{
		{DstConditionID: "0xB", Direction: DirAligned, Confidence: 1},
		{DstConditionID: "0xC", Direction: DirAligned, Confidence: 1},
	}
	in.WalletLines = []WalletLine{
		{ConditionID: "0xA", Side: "YES", NetSharesUSD: 10_000, LiquidityFloor: 5_000},
		{ConditionID: "0xB", Side: "YES", NetSharesUSD: 10_000, LiquidityFloor: 5_000},
		{ConditionID: "0xC", Side: "YES", NetSharesUSD: 10_000, LiquidityFloor: 5_000},
	}

	dNoCat := New(Config{}).Decide(in)
	in.Catalysts = []Catalyst{
		{Kind: "election_day", ExpectedAt: in.Now.Add(7 * 24 * time.Hour), Confidence: 0.9},
	}
	dWithCat := New(Config{}).Decide(in)
	if dWithCat.Score <= dNoCat.Score {
		t.Errorf("catalyst within window must raise score: noCat=%v withCat=%v", dNoCat.Score, dWithCat.Score)
	}
}
