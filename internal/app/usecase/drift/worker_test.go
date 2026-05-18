package drift

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeAlerts struct {
	candidates []repository.Alert
	updates    []repository.DriftUpdate
}

func (f *fakeAlerts) ListSentForDrift(_ context.Context, _ time.Duration, _ int32) ([]repository.Alert, error) {
	return f.candidates, nil
}

func (f *fakeAlerts) MarkDrift(_ context.Context, u repository.DriftUpdate) error {
	f.updates = append(f.updates, u)
	return nil
}

type fakePrices struct {
	// keyed by "<marketID>:<token>:<unixMilli>"
	at map[string]float64
}

func (f *fakePrices) key(market int64, token string, at time.Time) string {
	// Coarse round to the second — tests inject prices on whole seconds.
	return formatPriceKey(market, token, at)
}

func (f *fakePrices) TradePriceAtOrAfter(_ context.Context, market int64, token string, at time.Time) (float64, bool, error) {
	if p, ok := f.at[f.key(market, token, at)]; ok {
		return p, true, nil
	}
	return 0, false, nil
}

func formatPriceKey(market int64, token string, at time.Time) string {
	return token + "@" + at.UTC().Format(time.RFC3339)
}

func mustFinding(t *testing.T, outcomeToken string, side trade.Side, tradeAt time.Time, tradePrice float64) []byte {
	t.Helper()
	// Use an accumulation Finding payload because it carries OutcomeToken
	// (TradeRef does not). The drift worker reads OutcomeToken from there.
	f := anomaly.Finding{
		Trade: &anomaly.TradeRef{
			Side: side, Price: tradePrice, At: tradeAt,
		},
		Accumulation: &anomaly.AccumulationRef{
			OutcomeToken: outcomeToken,
		},
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mid(id int64) *int64 { return &id }

func newWorker(t *testing.T, alerts AlertStore, prices PriceLookup, now time.Time) *Worker {
	t.Helper()
	log := zerolog.Nop()
	return New(Config{
		Interval:      time.Minute,
		ClaimLimit:    64,
		LongestWindow: 24 * time.Hour,
		Clock:         func() time.Time { return now },
	}, alerts, prices, &log)
}

// TestTick_BUYFavorableDrift pins the canonical case: a BUY @ price 0.05
// that later trades at 0.06 → +0.2 (20% favourable for the buyer).
func TestTick_BUYFavorableDrift(t *testing.T) {
	tradeAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := tradeAt.Add(25 * time.Hour) // all windows elapsed
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 1, MarketID: mid(7), Payload: mustFinding(t, "tok-yes", trade.SideBuy, tradeAt, 0.05)},
	}}
	prices := &fakePrices{at: map[string]float64{
		formatPriceKey(7, "tok-yes", tradeAt.Add(15*time.Minute)): 0.06,
		formatPriceKey(7, "tok-yes", tradeAt.Add(1*time.Hour)):    0.07,
		formatPriceKey(7, "tok-yes", tradeAt.Add(6*time.Hour)):    0.08,
		formatPriceKey(7, "tok-yes", tradeAt.Add(24*time.Hour)):   0.10,
	}}
	w := newWorker(t, alerts, prices, now)

	w.Tick(context.Background())

	if len(alerts.updates) != 1 {
		t.Fatalf("expected 1 drift update, got %d", len(alerts.updates))
	}
	u := alerts.updates[0]
	if u.Status != repository.DriftAvailable {
		t.Errorf("status: got %s want available", u.Status)
	}
	if u.CLV15m == nil || math.Abs(*u.CLV15m-0.2) > 1e-9 {
		t.Errorf("clv_15m: %v want +0.2", u.CLV15m)
	}
	if u.CLV24h == nil || math.Abs(*u.CLV24h-1.0) > 1e-9 {
		t.Errorf("clv_24h: %v want +1.0", u.CLV24h)
	}
}

// TestTick_SELLFavorableDrift pins SELL semantics: SELL @ 0.05 → later
// 0.03 → +0.4 favourable for the seller (price dropped).
func TestTick_SELLFavorableDrift(t *testing.T) {
	tradeAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := tradeAt.Add(25 * time.Hour)
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 2, MarketID: mid(7), Payload: mustFinding(t, "tok-yes", trade.SideSell, tradeAt, 0.05)},
	}}
	prices := &fakePrices{at: map[string]float64{
		formatPriceKey(7, "tok-yes", tradeAt.Add(15*time.Minute)): 0.03,
	}}
	w := newWorker(t, alerts, prices, now)

	w.Tick(context.Background())

	if math.Abs(*alerts.updates[0].CLV15m-0.4) > 1e-9 {
		t.Errorf("sell clv_15m: %v want +0.4", alerts.updates[0].CLV15m)
	}
}

// TestTick_UnfavorableDrift pins the sign: a BUY whose price dropped
// gets a negative drift.
func TestTick_UnfavorableDrift(t *testing.T) {
	tradeAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := tradeAt.Add(25 * time.Hour)
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 3, MarketID: mid(7), Payload: mustFinding(t, "tok-yes", trade.SideBuy, tradeAt, 0.10)},
	}}
	prices := &fakePrices{at: map[string]float64{
		formatPriceKey(7, "tok-yes", tradeAt.Add(15*time.Minute)): 0.05,
	}}
	w := newWorker(t, alerts, prices, now)

	w.Tick(context.Background())

	if alerts.updates[0].CLV15m == nil || *alerts.updates[0].CLV15m >= 0 {
		t.Fatalf("expected negative drift, got %v", alerts.updates[0].CLV15m)
	}
}

// TestTick_PendingUntilLongestWindowElapsed pins the no-look-ahead rule:
// an alert that just fired (now-tradeAt < 15m) gets NO drift values and
// stays drift_status=pending.
func TestTick_PendingUntilLongestWindowElapsed(t *testing.T) {
	tradeAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := tradeAt.Add(5 * time.Minute) // before 15m window
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 4, MarketID: mid(7), Payload: mustFinding(t, "tok-yes", trade.SideBuy, tradeAt, 0.05)},
	}}
	prices := &fakePrices{at: map[string]float64{}}
	w := newWorker(t, alerts, prices, now)

	w.Tick(context.Background())

	if alerts.updates[0].Status != repository.DriftPending {
		t.Errorf("status: got %s want pending", alerts.updates[0].Status)
	}
	for i, p := range []*float64{
		alerts.updates[0].CLV15m, alerts.updates[0].CLV1h,
		alerts.updates[0].CLV6h, alerts.updates[0].CLV24h,
	} {
		if p != nil {
			t.Errorf("window %d should be nil before elapsing, got %v", i, *p)
		}
	}
}

// TestTick_PartialAvailability pins: when only 15m elapsed, 15m is
// populated and the rest stay nil. Status flips to available because at
// least one window produced a number.
func TestTick_PartialAvailability(t *testing.T) {
	tradeAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := tradeAt.Add(20 * time.Minute)
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 5, MarketID: mid(7), Payload: mustFinding(t, "tok-yes", trade.SideBuy, tradeAt, 0.05)},
	}}
	prices := &fakePrices{at: map[string]float64{
		formatPriceKey(7, "tok-yes", tradeAt.Add(15*time.Minute)): 0.06,
	}}
	w := newWorker(t, alerts, prices, now)

	w.Tick(context.Background())

	u := alerts.updates[0]
	if u.Status != repository.DriftAvailable {
		t.Errorf("status: got %s want available", u.Status)
	}
	if u.CLV15m == nil {
		t.Fatal("clv_15m must be populated")
	}
	if u.CLV1h != nil || u.CLV6h != nil || u.CLV24h != nil {
		t.Errorf("later windows must be nil: 1h=%v 6h=%v 24h=%v", u.CLV1h, u.CLV6h, u.CLV24h)
	}
}

// TestTick_NoReferencePriceMarksUnavailable pins: old alert (longest
// window elapsed) with no later trades available stamps unavailable.
func TestTick_NoReferencePriceMarksUnavailable(t *testing.T) {
	tradeAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	now := tradeAt.Add(25 * time.Hour)
	alerts := &fakeAlerts{candidates: []repository.Alert{
		{ID: 6, MarketID: mid(7), Payload: mustFinding(t, "tok-yes", trade.SideBuy, tradeAt, 0.05)},
	}}
	prices := &fakePrices{at: map[string]float64{}} // no later trades at all
	w := newWorker(t, alerts, prices, now)

	w.Tick(context.Background())

	if alerts.updates[0].Status != repository.DriftUnavailable {
		t.Errorf("status: got %s want unavailable", alerts.updates[0].Status)
	}
}
