package strategyvalue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRows struct {
	rows []PendingRow
	err  error
}

func (f *fakeRows) ListPendingValueRows(_ context.Context, _ time.Duration, _ int) ([]PendingRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakePrices struct {
	priceAt map[string]float64 // key = conditionID|outcome|RFC3339
	missing bool
}

func (f *fakePrices) FirstPriceAtOrAfter(_ context.Context, cid, ot string, at time.Time) (float64, bool, error) {
	if f.missing {
		return 0, false, nil
	}
	k := cid + "|" + ot + "|" + at.UTC().Format(time.RFC3339)
	if v, ok := f.priceAt[k]; ok {
		return v, true, nil
	}
	return 0, false, nil
}

type fakeUpdater struct {
	mu      sync.Mutex
	updates map[int64]Values
}

func (u *fakeUpdater) UpdateValues(_ context.Context, id int64, v Values) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.updates == nil {
		u.updates = map[int64]Values{}
	}
	u.updates[id] = v
	return nil
}

func newCfg() Config {
	return Config{Enabled: true, Interval: time.Minute, BatchSize: 100, MaxAge: 30 * 24 * time.Hour}
}

func TestTick_SignedMovePositiveForBuyWhenRefAboveBaseline(t *testing.T) {
	fired := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	now := fired.Add(2 * time.Hour) // 15m + 1h elapsed; 6h/24h not yet
	rows := &fakeRows{rows: []PendingRow{{
		ID: 1, StrategyName: "thesisaccum",
		ConditionID: "cond-A", OutcomeToken: "Yes",
		Side: "BUY", BaselinePrice: 0.50,
		FiredAt: fired,
	}}}
	prices := &fakePrices{priceAt: map[string]float64{
		"cond-A|Yes|" + fired.Add(15*time.Minute).Format(time.RFC3339): 0.52,
		"cond-A|Yes|" + fired.Add(1*time.Hour).Format(time.RFC3339):    0.55,
	}}
	upd := &fakeUpdater{}
	w := New(newCfg(), rows, prices, upd, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())

	v, ok := upd.updates[1]
	if !ok {
		t.Fatalf("expected update for id=1")
	}
	if v.CLV15m == nil || *v.CLV15m <= 0 {
		t.Fatalf("expected positive CLV15m; got %v", v.CLV15m)
	}
	if v.CLV1h == nil || *v.CLV1h <= 0 {
		t.Fatalf("expected positive CLV1h; got %v", v.CLV1h)
	}
	if v.CLV6h != nil || v.CLV24h != nil {
		t.Fatalf("6h/24h should not be set yet: %+v", v)
	}
	if v.Reversal15m == nil || *v.Reversal15m {
		t.Fatalf("no reversal expected; got %+v", v.Reversal15m)
	}
}

func TestTick_AdverseMoveYieldsNegativeClv(t *testing.T) {
	fired := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	now := fired.Add(2 * time.Hour)
	rows := &fakeRows{rows: []PendingRow{{
		ID: 2, ConditionID: "cond-A", OutcomeToken: "Yes",
		Side: "BUY", BaselinePrice: 0.50, FiredAt: fired,
	}}}
	prices := &fakePrices{priceAt: map[string]float64{
		"cond-A|Yes|" + fired.Add(15*time.Minute).Format(time.RFC3339): 0.45,
		"cond-A|Yes|" + fired.Add(1*time.Hour).Format(time.RFC3339):    0.42,
	}}
	upd := &fakeUpdater{}
	w := New(newCfg(), rows, prices, upd, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())

	v := upd.updates[2]
	if v.CLV15m == nil || *v.CLV15m >= 0 {
		t.Fatalf("expected negative CLV15m on adverse move; got %v", v.CLV15m)
	}
}

func TestTick_SellSidePositiveWhenPriceDrops(t *testing.T) {
	fired := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	now := fired.Add(2 * time.Hour)
	rows := &fakeRows{rows: []PendingRow{{
		ID: 3, ConditionID: "cond-A", OutcomeToken: "Yes",
		Side: "SELL", BaselinePrice: 0.50, FiredAt: fired,
	}}}
	prices := &fakePrices{priceAt: map[string]float64{
		"cond-A|Yes|" + fired.Add(15*time.Minute).Format(time.RFC3339): 0.45,
	}}
	upd := &fakeUpdater{}
	w := New(newCfg(), rows, prices, upd, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())

	v := upd.updates[3]
	if v.CLV15m == nil || *v.CLV15m <= 0 {
		t.Fatalf("expected positive CLV15m for SELL when price dropped; got %v", v.CLV15m)
	}
}

func TestTick_MissingPriceLeavesNullDoesNotPanic(t *testing.T) {
	fired := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	now := fired.Add(2 * time.Hour)
	rows := &fakeRows{rows: []PendingRow{{ID: 4, FiredAt: fired, BaselinePrice: 0.5, Side: "BUY"}}}
	upd := &fakeUpdater{}
	w := New(newCfg(), rows, &fakePrices{missing: true}, upd, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())
	if _, ok := upd.updates[4]; ok {
		t.Fatalf("missing prices must skip update entirely")
	}
}

func TestTick_IdempotentSkipsAlreadyComputed(t *testing.T) {
	fired := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	now := fired.Add(2 * time.Hour)
	old := 1.5
	rows := &fakeRows{rows: []PendingRow{{
		ID: 5, ConditionID: "cond-A", OutcomeToken: "Yes",
		Side: "BUY", BaselinePrice: 0.5, FiredAt: fired,
		CLV15m: &old, // already filled
	}}}
	prices := &fakePrices{priceAt: map[string]float64{
		"cond-A|Yes|" + fired.Add(15*time.Minute).Format(time.RFC3339): 0.6,
		"cond-A|Yes|" + fired.Add(1*time.Hour).Format(time.RFC3339):    0.62,
	}}
	upd := &fakeUpdater{}
	w := New(newCfg(), rows, prices, upd, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())

	v := upd.updates[5]
	if v.CLV15m != nil {
		t.Fatalf("must not re-compute 15m when already filled; got %v", *v.CLV15m)
	}
	if v.CLV1h == nil {
		t.Fatalf("expected CLV1h still computed: %+v", v)
	}
}

func TestTick_BailsCleanlyOnError(t *testing.T) {
	w := New(newCfg(), &fakeRows{err: errors.New("db down")}, &fakePrices{}, &fakeUpdater{}, nil, nil)
	w.Tick(context.Background())
}

func TestTick_NoOpWhenDepsMissing(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil, nil)
	w.Tick(context.Background())
}

func TestTick_WindowsRequireElapsedTime(t *testing.T) {
	fired := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	// Only 5 minutes elapsed — no windows eligible.
	now := fired.Add(5 * time.Minute)
	rows := &fakeRows{rows: []PendingRow{{ID: 9, FiredAt: fired, BaselinePrice: 0.5, Side: "BUY"}}}
	upd := &fakeUpdater{}
	w := New(newCfg(), rows, &fakePrices{}, upd, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())
	if _, ok := upd.updates[9]; ok {
		t.Fatalf("no windows elapsed → no update")
	}
}
