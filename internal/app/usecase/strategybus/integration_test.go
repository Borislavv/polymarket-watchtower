package strategybus

import (
	"context"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/thesisaccum"
)

// TestEndToEnd_ThesisAccumDetectorThroughBus exercises the v11.5
// shadow-first path end-to-end: a real detector decides on
// synthetic inputs; the bus rewrites ShadowOnly and persists a row
// through the in-memory writer. This is the canonical shape every
// detector's orchestration call site (detect.Loop / a worker)
// should follow.
func TestEndToEnd_ThesisAccumDetectorThroughBus(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)

	det := thesisaccum.New(thesisaccum.Config{
		MinBreadth:      2,
		MinConsistency:  0.75,
		MinAlignedScore: 1.0,
	})
	in := thesisaccum.Input{
		SourceConditionID: "src",
		SourceEventSlug:   "ny-mayor",
		Wallet:            "0xabc",
		Side:              "YES",
		Now:               now,
		Links: []thesisaccum.Link{
			{DstConditionID: "alt-1", Direction: thesisaccum.DirAligned, Confidence: 0.9, LinkType: "same_event"},
			{DstConditionID: "alt-2", Direction: thesisaccum.DirAligned, Confidence: 0.9, LinkType: "same_event"},
		},
		WalletLines: []thesisaccum.WalletLine{
			{ConditionID: "src", Side: "YES", NetSharesUSD: 5000, Trades: 5, LiquidityFloor: 500, BaselineMedianUSD: 100, WindowStart: now.Add(-24 * time.Hour)},
			{ConditionID: "alt-1", Side: "YES", NetSharesUSD: 4500, Trades: 4, LiquidityFloor: 500, BaselineMedianUSD: 100, WindowStart: now.Add(-24 * time.Hour)},
			{ConditionID: "alt-2", Side: "YES", NetSharesUSD: 3000, Trades: 3, LiquidityFloor: 500, BaselineMedianUSD: 100, WindowStart: now.Add(-24 * time.Hour)},
		},
	}
	v := det.Decide(in)
	if !v.Fired {
		t.Fatalf("expected verdict to fire; got %+v", v)
	}

	bus := New(Config{
		StrategyVersion:        "v11.5-shadow",
		GlobalPromotionAllowed: false,
		Flags: map[string]StrategyFlag{
			"thesisaccum": {Name: "thesisaccum", Enabled: true, ShadowOnly: true},
		},
	}, &captureWriter{}, nil, nil).WithClock(func() time.Time { return now })

	// Adapt the verdict to a Decision. This is the boilerplate every
	// orchestration call site repeats; future iterations can factor
	// it into a helper.
	d := shadowdecisions.Decision{
		StrategyName:     "thesisaccum",
		ConditionID:      in.SourceConditionID,
		EventSlug:        in.SourceEventSlug,
		Wallet:           in.Wallet,
		Side:             in.Side,
		Kind:             shadowdecisions.KindStandalone,
		Level:            shadowdecisions.DecisionLevel(v.Level),
		Score:            v.Score,
		Confidence:       v.Confidence,
		Reasons:          v.Reasons,
		Features:         v.Features,
		ControlBucketKey: shadowdecisions.ControlBucketKey("politics", "75-100", "0.5-0.7", "1k-10k", "election"),
	}
	id, err := bus.Record(context.Background(), d)
	if err != nil {
		t.Fatalf("bus.Record: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id when writer succeeds")
	}
}
