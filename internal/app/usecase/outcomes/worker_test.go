package outcomes

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeAlerts struct {
	candidates []repository.Alert
	updates    []repository.OutcomeUpdate
	touches    []int64
}

func (f *fakeAlerts) ListSentForOutcomeCheck(_ context.Context, _ int32) ([]repository.Alert, error) {
	return f.candidates, nil
}

func (f *fakeAlerts) MarkOutcome(_ context.Context, u repository.OutcomeUpdate) error {
	f.updates = append(f.updates, u)
	return nil
}

func (f *fakeAlerts) TouchOutcomeUnavailable(_ context.Context, id int64) error {
	f.touches = append(f.touches, id)
	return nil
}

type fakeMarkets struct {
	byID map[int64]repository.Market
	err  error
}

func (f *fakeMarkets) GetByID(_ context.Context, id int64) (repository.Market, error) {
	if f.err != nil {
		return repository.Market{}, f.err
	}
	m, ok := f.byID[id]
	if !ok {
		return repository.Market{}, errors.New("not found")
	}
	return m, nil
}

type fakeGamma struct {
	byCondition map[string]gamma.MarketResolution
	missing     map[string]bool
	err         error
}

func (f *fakeGamma) GetMarketResolution(_ context.Context, conditionID string) (gamma.MarketResolution, bool, error) {
	if f.err != nil {
		return gamma.MarketResolution{}, false, f.err
	}
	if f.missing[conditionID] {
		return gamma.MarketResolution{}, false, nil
	}
	res, ok := f.byCondition[conditionID]
	return res, ok, nil
}

func mustFinding(t *testing.T, outcomeLabel string, side trade.Side) []byte {
	t.Helper()
	f := anomaly.Finding{
		Trade: &anomaly.TradeRef{Outcome: outcomeLabel, Side: side},
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal finding: %v", err)
	}
	return b
}

func newWorker(t *testing.T, alerts AlertStore, markets MarketLookup, g MarketResolver) *Worker {
	t.Helper()
	log := zerolog.Nop()
	return New(Config{
		Interval:              time.Minute,
		ClaimLimit:            64,
		WinningPriceThreshold: 0.99,
	}, alerts, markets, g, &log)
}

func mid(id int64) *int64 { return &id }

// TestTick_UnresolvedMarketTouchesOnly pins: a sent alert on a market
// that hasn't closed upstream yet should be touched (so it's re-checked
// later) but NOT classified.
func TestTick_UnresolvedMarketTouchesOnly(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 1, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideBuy)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{byCondition: map[string]gamma.MarketResolution{
		"0xa": {ConditionID: "0xa", Closed: false},
	}}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if len(alerts.updates) != 0 {
		t.Fatalf("unresolved market must not be classified: %+v", alerts.updates)
	}
	if len(alerts.touches) != 1 || alerts.touches[0] != 1 {
		t.Errorf("expected touch of alert 1, got %v", alerts.touches)
	}
}

// TestTick_ResolvedCorrectBUY pins: BUY Yes + Yes wins → correct.
func TestTick_ResolvedCorrectBUY(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 11, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideBuy)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{byCondition: map[string]gamma.MarketResolution{
		"0xa": {
			ConditionID:   "0xa",
			Closed:        true,
			EndDate:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			TokenIDs:      []string{"tok-yes", "tok-no"},
			OutcomeLabels: []string{"Yes", "No"},
			OutcomePrices: []float64{1.0, 0.0},
		},
	}}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if len(alerts.updates) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(alerts.updates))
	}
	if alerts.updates[0].Status != repository.OutcomeCorrect {
		t.Errorf("status: got %s want resolved_correct", alerts.updates[0].Status)
	}
	if alerts.updates[0].WinningOutcomeLabel != "Yes" {
		t.Errorf("winning label: %s", alerts.updates[0].WinningOutcomeLabel)
	}
}

// TestTick_ResolvedWrongBUY pins: BUY Yes + No wins → wrong.
func TestTick_ResolvedWrongBUY(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 12, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideBuy)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{byCondition: map[string]gamma.MarketResolution{
		"0xa": {
			ConditionID:   "0xa",
			Closed:        true,
			TokenIDs:      []string{"tok-yes", "tok-no"},
			OutcomeLabels: []string{"Yes", "No"},
			OutcomePrices: []float64{0.0, 1.0},
		},
	}}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if alerts.updates[0].Status != repository.OutcomeWrong {
		t.Errorf("status: got %s want resolved_wrong", alerts.updates[0].Status)
	}
}

// TestTick_SellLosingOutcomeIsCorrect pins the SELL semantics: selling
// the losing side is a correct read.
func TestTick_SellLosingOutcomeIsCorrect(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 13, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideSell)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{byCondition: map[string]gamma.MarketResolution{
		"0xa": {
			ConditionID:   "0xa",
			Closed:        true,
			TokenIDs:      []string{"tok-yes", "tok-no"},
			OutcomeLabels: []string{"Yes", "No"},
			OutcomePrices: []float64{0.0, 1.0}, // No wins → Sell Yes was right
		},
	}}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if alerts.updates[0].Status != repository.OutcomeCorrect {
		t.Errorf("Sell Yes + No-wins must be correct, got %s", alerts.updates[0].Status)
	}
}

// TestTick_InconclusiveResolutionMarksUnknown pins the failsafe: a closed
// market with no clear winner (prices all under 0.99) is stamped unknown.
func TestTick_InconclusiveResolutionMarksUnknown(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 14, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideBuy)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{byCondition: map[string]gamma.MarketResolution{
		"0xa": {
			ConditionID:   "0xa",
			Closed:        true,
			TokenIDs:      []string{"tok-yes", "tok-no"},
			OutcomeLabels: []string{"Yes", "No"},
			OutcomePrices: []float64{0.5, 0.5},
		},
	}}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if alerts.updates[0].Status != repository.OutcomeUnknown {
		t.Errorf("inconclusive resolution must mark unknown, got %s", alerts.updates[0].Status)
	}
}

// TestTick_GammaMissingMarksUnavailable pins: a market that Gamma can't
// find any more (archived / unknown) becomes unavailable, not pending.
func TestTick_GammaMissingMarksUnavailable(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 15, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideBuy)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{missing: map[string]bool{"0xa": true}}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if len(alerts.updates) != 1 || alerts.updates[0].Status != repository.OutcomeUnavailable {
		t.Fatalf("expected unavailable verdict, got %+v", alerts.updates)
	}
}

// TestTick_TransientErrorTouches pins: a Gamma transient error keeps the
// row touched-but-pending so the next tick retries.
func TestTick_TransientErrorTouches(t *testing.T) {
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 16, MarketID: mid(7), Payload: mustFinding(t, "Yes", trade.SideBuy)},
	}}
	markets := &fakeMarkets{byID: map[int64]repository.Market{7: {ID: 7, ConditionID: "0xa"}}}
	g := &fakeGamma{err: errors.New("gamma 502")}
	w := newWorker(t, alerts, markets, g)

	w.Tick(context.Background())

	if len(alerts.updates) != 0 {
		t.Fatalf("transient error must not classify, got %+v", alerts.updates)
	}
	if len(alerts.touches) != 1 {
		t.Errorf("expected touch on transient error, got %v", alerts.touches)
	}
}
