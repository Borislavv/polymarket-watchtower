package strategyoutcome

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeLister struct {
	rows []PendingRow
	err  error
}

func (f *fakeLister) ListShadowRowsForOutcomeBackfill(_ context.Context, _ int) ([]PendingRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeUpdater struct {
	mu      sync.Mutex
	updates map[int64]string
	failOn  int64
}

func (u *fakeUpdater) UpdateShadowOutcomeStatus(_ context.Context, id int64, status string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failOn == id {
		return errors.New("synthetic failure")
	}
	if u.updates == nil {
		u.updates = map[int64]string{}
	}
	u.updates[id] = status
	return nil
}

func newCfg() Config {
	return Config{Enabled: true, Interval: time.Hour, BatchSize: 1000}
}

func TestTick_BackfillsResolvedRows(t *testing.T) {
	l := &fakeLister{rows: []PendingRow{
		{ID: 1, AlertOutcome: "resolved_correct"},
		{ID: 2, AlertOutcome: "resolved_wrong"},
	}}
	u := &fakeUpdater{}
	w := New(newCfg(), l, u, nil, nil)
	w.Tick(context.Background())
	if got, want := len(u.updates), 2; got != want {
		t.Fatalf("expected %d updates; got %d", want, got)
	}
	if u.updates[1] != "resolved_correct" || u.updates[2] != "resolved_wrong" {
		t.Fatalf("wrong updates: %+v", u.updates)
	}
}

func TestTick_EmptyOutcomeSkipped(t *testing.T) {
	l := &fakeLister{rows: []PendingRow{{ID: 3, AlertOutcome: ""}}}
	u := &fakeUpdater{}
	w := New(newCfg(), l, u, nil, nil)
	w.Tick(context.Background())
	if len(u.updates) != 0 {
		t.Fatalf("must not write empty outcomes")
	}
}

func TestTick_UpdateFailureDoesNotStop(t *testing.T) {
	l := &fakeLister{rows: []PendingRow{
		{ID: 1, AlertOutcome: "resolved_correct"},
		{ID: 2, AlertOutcome: "resolved_wrong"},
	}}
	u := &fakeUpdater{failOn: 1}
	w := New(newCfg(), l, u, nil, nil)
	w.Tick(context.Background())
	if len(u.updates) != 1 || u.updates[2] != "resolved_wrong" {
		t.Fatalf("expected row 2 updated; got %+v", u.updates)
	}
}

func TestTick_ListErrorBailsCleanly(t *testing.T) {
	l := &fakeLister{err: errors.New("db down")}
	u := &fakeUpdater{}
	w := New(newCfg(), l, u, nil, nil)
	w.Tick(context.Background())
	if len(u.updates) != 0 {
		t.Fatalf("must not update on list error")
	}
}

func TestTick_DepsMissingNoOp(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	w.Tick(context.Background()) // must not panic
}
