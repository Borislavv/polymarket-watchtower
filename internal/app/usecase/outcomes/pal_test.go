package outcomes

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// TestImpliedProbability_BUYAndSELLSymmetry pins the SELL inversion:
// the wallet's implied probability of being directionally correct is
// price for BUY, (1-price) for SELL.
func TestImpliedProbability_BUYAndSELLSymmetry(t *testing.T) {
	cases := []struct {
		name     string
		price    float64
		side     trade.Side
		wantProb float64
	}{
		{"BUY @ 0.10 → 0.10", 0.10, trade.SideBuy, 0.10},
		{"BUY @ 0.50 → 0.50", 0.50, trade.SideBuy, 0.50},
		{"BUY @ 0.90 → 0.90", 0.90, trade.SideBuy, 0.90},
		{"SELL @ 0.10 → 0.90", 0.10, trade.SideSell, 0.90},
		{"SELL @ 0.50 → 0.50", 0.50, trade.SideSell, 0.50},
		{"SELL @ 0.90 → 0.10", 0.90, trade.SideSell, 0.10},
		// Defensive clamps:
		{"BUY @ -0.5 (clamped to 0)", -0.5, trade.SideBuy, 0},
		{"BUY @ 1.5 (clamped to 1)", 1.5, trade.SideBuy, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ImpliedProbability(tc.price, tc.side)
			// Float arithmetic on 1-0.9 yields 0.09999999999999998 —
			// use a tight tolerance instead of bit-equality.
			if got < tc.wantProb-1e-9 || got > tc.wantProb+1e-9 {
				t.Errorf("got %v want %v", got, tc.wantProb)
			}
		})
	}
}

// TestRealizedEdge_AllResolvedCombinations pins the spec example
// matrix: success at odds 2.0 → +0.50, success at odds 8.0 → +0.875,
// failure at odds 8.0 → -0.125. Edge calculus is the load-bearing
// proof-of-value metric — silent regressions here would invalidate
// the whole reporting layer.
func TestRealizedEdge_AllResolvedCombinations(t *testing.T) {
	cases := []struct {
		name        string
		status      repository.OutcomeStatus
		impliedProb float64
		wantEdge    float64
		wantValid   bool
	}{
		// Spec examples (interpreted: "odds 2.0" → price 0.50 → implied=0.50)
		{"success @ implied 0.50 (odds 2)", repository.OutcomeCorrect, 0.50, +0.50, true},
		{"success @ implied 0.125 (odds 8)", repository.OutcomeCorrect, 0.125, +0.875, true},
		{"failure @ implied 0.125 (odds 8)", repository.OutcomeWrong, 0.125, -0.125, true},
		// Long-shot extremes
		{"success @ implied 0.05", repository.OutcomeCorrect, 0.05, +0.95, true},
		{"failure @ implied 0.95 (broken chalk)", repository.OutcomeWrong, 0.95, -0.95, true},
		// Non-resolved verdicts return EdgeValid=false (caller must
		// skip the edge histogram).
		{"pending → invalid", repository.OutcomePending, 0.30, 0, false},
		{"unknown → invalid", repository.OutcomeUnknown, 0.30, 0, false},
		{"unavailable → invalid", repository.OutcomeUnavailable, 0.30, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edge, ok := RealizedEdge(tc.status, tc.impliedProb)
			if ok != tc.wantValid {
				t.Fatalf("ok: got %v want %v", ok, tc.wantValid)
			}
			if !ok {
				return
			}
			if edge < tc.wantEdge-1e-9 || edge > tc.wantEdge+1e-9 {
				t.Errorf("edge: got %v want %v", edge, tc.wantEdge)
			}
		})
	}
}

// TestCalibrationBucket_BoundariesAreLeftInclusive pins the bucket
// edges. 10% lands in "10-20" (not "0-10"); 70% lands in "70+".
func TestCalibrationBucket_BoundariesAreLeftInclusive(t *testing.T) {
	cases := []struct {
		prob       float64
		wantBucket string
	}{
		{0.00, "0-10"},
		{0.09, "0-10"},
		{0.10, "10-20"},
		{0.19, "10-20"},
		{0.20, "20-30"},
		{0.29, "20-30"},
		{0.30, "30-40"},
		{0.40, "40-50"},
		{0.50, "50-70"},
		{0.69, "50-70"},
		{0.70, "70+"},
		{0.95, "70+"},
		{1.00, "70+"},
	}
	for _, tc := range cases {
		t.Run(tc.wantBucket, func(t *testing.T) {
			if got := CalibrationBucket(tc.prob); got != tc.wantBucket {
				t.Errorf("prob=%v: got %q want %q", tc.prob, got, tc.wantBucket)
			}
		})
	}
}

// TestSeverityWeight_KnownAndUnknown pins the weight map.
func TestSeverityWeight_KnownAndUnknown(t *testing.T) {
	cases := map[string]float64{
		"info":     1,
		"warning":  3,
		"critical": 10,
		"hard":     25,
		"":         0, // misconfigured emission
		"bogus":    0,
	}
	for sev, want := range cases {
		t.Run(sev, func(t *testing.T) {
			if got := SeverityWeight(sev); got != want {
				t.Errorf("got %v want %v", got, want)
			}
		})
	}
}

// TestBuildSnapshot_HappyPath wires the pieces: a BUY at 0.08 that
// resolved correctly produces edge=+0.92, bucket="0-10" (left-
// inclusive upper bound; see CalibrationBucket boundary test),
// success=1, weight=1 (info).
func TestBuildSnapshot_HappyPath(t *testing.T) {
	snap := BuildSnapshot(
		repository.OutcomeCorrect,
		"info", "trade_anomaly",
		0.08, trade.SideBuy,
	)
	if !snap.EdgeValid {
		t.Fatal("EdgeValid: got false")
	}
	if snap.Edge < 0.919 || snap.Edge > 0.921 {
		t.Errorf("Edge: got %v want ~0.92", snap.Edge)
	}
	if snap.Bucket != "0-10" {
		t.Errorf("Bucket: got %q want 0-10", snap.Bucket)
	}
	if snap.Weight != 1 {
		t.Errorf("Weight: got %v want 1", snap.Weight)
	}
	if snap.SuccessBinary != 1 {
		t.Errorf("SuccessBinary: got %v want 1", snap.SuccessBinary)
	}
}

// TestBuildSnapshot_PendingStillProducesBucketAndWeight confirms
// pending alerts still contribute to the calibration counter (so
// operators can see "the 0-10% bucket has 200 alerts, 50 pending")
// without polluting the edge / weighted-success metrics.
func TestBuildSnapshot_PendingStillProducesBucketAndWeight(t *testing.T) {
	snap := BuildSnapshot(
		repository.OutcomePending,
		"critical", "ownership_concentration",
		0.05, trade.SideBuy,
	)
	if snap.EdgeValid {
		t.Error("EdgeValid: pending must be false")
	}
	if snap.Bucket != "0-10" {
		t.Errorf("Bucket: got %q want 0-10", snap.Bucket)
	}
	if snap.Weight != 10 {
		t.Errorf("Weight: critical must be 10, got %v", snap.Weight)
	}
}

// TestBuildSnapshot_SellSideInversion confirms a SELL alert that
// "won" (the seller's prediction came true) produces a positive edge.
// SELL YES @ 0.92 → wallet thinks NO will win at implied 0.08.
// Resolved_correct → edge = 1 - 0.08 = +0.92, bucket = "0-10"
// (left-inclusive upper bound).
func TestBuildSnapshot_SellSideInversion(t *testing.T) {
	snap := BuildSnapshot(
		repository.OutcomeCorrect,
		"warning", "accumulation",
		0.92, trade.SideSell,
	)
	if !snap.EdgeValid {
		t.Fatal("EdgeValid: got false")
	}
	if snap.Edge < 0.919 || snap.Edge > 0.921 {
		t.Errorf("Edge: got %v want ~0.92", snap.Edge)
	}
	if snap.Bucket != "0-10" {
		t.Errorf("Bucket should reflect inverted implied prob: got %q want 0-10", snap.Bucket)
	}
}
