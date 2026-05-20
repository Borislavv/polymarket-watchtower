package repricing

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// fakeTrades returns rows the provider would have seen.
type fakeTrades struct {
	calls []sqlc.SumConditionTradesInWindowParams
	resp  []sqlc.SumConditionTradesInWindowRow
}

func (f *fakeTrades) SumConditionTradesInWindow(_ context.Context, p sqlc.SumConditionTradesInWindowParams) ([]sqlc.SumConditionTradesInWindowRow, error) {
	f.calls = append(f.calls, p)
	return f.resp, nil
}

// fakeStore captures upserts for verification.
type fakeStore struct {
	upserts []repository.NewRepricingSignal
}

func (f *fakeStore) UpsertRepricingSignal(_ context.Context, s repository.NewRepricingSignal) error {
	f.upserts = append(f.upserts, s)
	return nil
}

func ptr(f float64) *float64 { return &f }

func TestCompute_AlreadyPriced_WhenCurrentNearPriceAfter(t *testing.T) {
	in := AnnotationInput{
		EventSlug: "tx", ConditionID: "0xa", AnnotationHash: "h1",
		Title: "poll", Timestamp: time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC),
		PriceBefore: ptr(0.50), PriceAfter: ptr(0.62), CurrentPrice: ptr(0.625),
	}
	trades := &fakeTrades{} // no flow rows
	store := &fakeStore{}
	p := New(Config{Enabled: true}, trades, store, nil, nil)
	sig, _ := p.Compute(context.Background(), in, true)
	if sig.RepricingStatus != StatusAlreadyPriced {
		t.Fatalf("expected already_priced, got %q (%s)", sig.RepricingStatus, sig.Explanation)
	}
	if len(store.upserts) != 1 {
		t.Errorf("persist must run when persist=true")
	}
}

func TestCompute_Underreacting_WhenCurrentBelowPriceAfter(t *testing.T) {
	in := AnnotationInput{
		EventSlug: "tx", ConditionID: "0xa", AnnotationHash: "h2",
		Title: "scandal", Timestamp: time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC),
		PriceBefore: ptr(0.50), PriceAfter: ptr(0.62), CurrentPrice: ptr(0.55),
	}
	p := New(Config{Enabled: true}, &fakeTrades{}, &fakeStore{}, nil, nil)
	sig, _ := p.Compute(context.Background(), in, false)
	if sig.RepricingStatus != StatusUnderreacting {
		t.Fatalf("expected underreacting, got %q", sig.RepricingStatus)
	}
}

func TestCompute_Reversed_WhenCurrentDropsBelowPriceBefore(t *testing.T) {
	in := AnnotationInput{
		EventSlug: "tx", ConditionID: "0xa", AnnotationHash: "h3",
		Title: "rumor disproved", Timestamp: time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC),
		PriceBefore: ptr(0.50), PriceAfter: ptr(0.62), CurrentPrice: ptr(0.45),
	}
	p := New(Config{Enabled: true}, &fakeTrades{}, &fakeStore{}, nil, nil)
	sig, _ := p.Compute(context.Background(), in, false)
	if sig.RepricingStatus != StatusReversed {
		t.Fatalf("expected reversed, got %q (%s)", sig.RepricingStatus, sig.Explanation)
	}
}

func TestCompute_FlowTiming_PostEventChasing(t *testing.T) {
	now := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	in := AnnotationInput{
		EventSlug: "tx", ConditionID: "0xa", AnnotationHash: "h4",
		Title: "x", Timestamp: now,
		PriceBefore: ptr(0.50), PriceAfter: ptr(0.62), CurrentPrice: ptr(0.62),
	}
	// trades will be answered identically for both pre- and post-
	// windows; we control which by inspecting params and returning
	// different sums.
	trades := &fakeTradesWindowed{
		// pre-window: small flow.
		preSame: 100, preOpp: 0,
		// post-window: large same-side flow → chasing.
		postSame: 50_000, postOpp: 0,
		annoT: pgtype.Timestamptz{Time: now, Valid: true},
	}
	p := New(Config{Enabled: true, PreWindow: 2 * time.Hour, PostWindow: 2 * time.Hour}, trades, &fakeStore{}, nil, nil)
	sig, _ := p.Compute(context.Background(), in, false)
	if sig.FlowTiming != FlowTimingPostEvent {
		t.Errorf("expected post_event_chasing, got %q (pre=%v post=%v)", sig.FlowTiming, sig.PreAnnotationFlowUSD, sig.PostAnnotationFlowUSD)
	}
}

func TestCompute_FlowTiming_PreEventPositioning(t *testing.T) {
	now := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	in := AnnotationInput{
		EventSlug: "tx", ConditionID: "0xa", AnnotationHash: "h5",
		Title: "x", Timestamp: now,
		PriceBefore: ptr(0.50), PriceAfter: ptr(0.62), CurrentPrice: ptr(0.62),
	}
	trades := &fakeTradesWindowed{
		preSame: 50_000, preOpp: 0, // big pre-flow
		postSame: 200, postOpp: 0, // tiny post-flow
		annoT: pgtype.Timestamptz{Time: now, Valid: true},
	}
	p := New(Config{Enabled: true, PreWindow: 2 * time.Hour, PostWindow: 2 * time.Hour}, trades, &fakeStore{}, nil, nil)
	sig, _ := p.Compute(context.Background(), in, false)
	if sig.FlowTiming != FlowTimingPreEvent {
		t.Errorf("expected pre_event_positioning, got %q", sig.FlowTiming)
	}
}

func TestCompute_OppositeSideFlow_ContradictionSurface(t *testing.T) {
	now := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	in := AnnotationInput{
		EventSlug: "tx", ConditionID: "0xa", AnnotationHash: "h6",
		Title: "x", Timestamp: now,
		PriceBefore: ptr(0.50), PriceAfter: ptr(0.62), CurrentPrice: ptr(0.61),
	}
	trades := &fakeTradesWindowed{
		preSame: 0, preOpp: 0,
		postSame: 5_000, postOpp: 25_000, // opposite-side resistance
		annoT: pgtype.Timestamptz{Time: now, Valid: true},
	}
	p := New(Config{Enabled: true, PreWindow: 2 * time.Hour, PostWindow: 2 * time.Hour}, trades, &fakeStore{}, nil, nil)
	sig, _ := p.Compute(context.Background(), in, false)
	// Opposite-side > same-side at this magnitude → already_priced
	// or still_repricing (no same-side dominance). Either way the
	// numbers must surface so the AI can reason.
	if sig.OppositeSidePostFlowUSD != 25_000 || sig.SameSidePostFlowUSD != 5_000 {
		t.Errorf("opposite-side numbers must surface: %+v", sig)
	}
}

// --- helpers --------------------------------------------------------------

type fakeTradesWindowed struct {
	preSame, preOpp   float64
	postSame, postOpp float64
	annoT             pgtype.Timestamptz
}

func (f *fakeTradesWindowed) SumConditionTradesInWindow(_ context.Context, p sqlc.SumConditionTradesInWindowParams) ([]sqlc.SumConditionTradesInWindowRow, error) {
	// Pre-window queries have until == annotation time; post-window
	// have since == annotation time.
	if p.Until.Valid && p.Until.Time.Equal(f.annoT.Time) {
		return []sqlc.SumConditionTradesInWindowRow{
			{Side: "BUY", NotionalUsd: f.preSame, TradeCount: 1},
			{Side: "SELL", NotionalUsd: f.preOpp, TradeCount: 1},
		}, nil
	}
	return []sqlc.SumConditionTradesInWindowRow{
		{Side: "BUY", NotionalUsd: f.postSame, TradeCount: 1},
		{Side: "SELL", NotionalUsd: f.postOpp, TradeCount: 1},
	}, nil
}

// Compile-time assertions that fakes satisfy the seams.
var (
	_ TradeWindowQuerier = (*fakeTrades)(nil)
	_ TradeWindowQuerier = (*fakeTradesWindowed)(nil)
	_ Store              = (*fakeStore)(nil)
)
