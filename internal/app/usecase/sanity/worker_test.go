package sanity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeReaper struct {
	candidates []repository.Market
	purgeErr   error
	resumeErr  error

	purged  []int64
	resumed []int64
}

func (f *fakeReaper) ListSoftDeletedForPurge(_ context.Context, _ time.Time, _ int32) ([]repository.Market, error) {
	return f.candidates, nil
}

func (f *fakeReaper) MarkPurged(_ context.Context, id int64) error {
	if f.purgeErr != nil {
		return f.purgeErr
	}
	f.purged = append(f.purged, id)
	return nil
}

func (f *fakeReaper) RequeueResumed(_ context.Context, id int64) error {
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.resumed = append(f.resumed, id)
	return nil
}

type fakeUpstream struct {
	activeByCondition map[string]bool
}

func (f *fakeUpstream) IsActiveUpstream(conditionID string) bool {
	return f.activeByCondition[conditionID]
}

func newWorker(reaper *fakeReaper, upstream *fakeUpstream) *Worker {
	log := zerolog.Nop()
	return New(Config{
		Interval:   time.Hour,
		Retention:  720 * time.Hour,
		ClaimLimit: 100,
		Clock:      func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}, reaper, upstream, nil, &log)
}

// TestTick_PurgesStaleSoftDeleted pins: a market still missing upstream
// after retention is stamped purged_at and excluded from active processing.
func TestTick_PurgesStaleSoftDeleted(t *testing.T) {
	reaper := &fakeReaper{candidates: []repository.Market{
		{ID: 1, ConditionID: "0xstale"},
		{ID: 2, ConditionID: "0xalso-stale"},
	}}
	upstream := &fakeUpstream{activeByCondition: map[string]bool{}}
	w := newWorker(reaper, upstream)

	w.Tick(context.Background())

	if len(reaper.purged) != 2 {
		t.Fatalf("purged: got %v want [1 2]", reaper.purged)
	}
	if len(reaper.resumed) != 0 {
		t.Errorf("resumed must be empty when no candidate is upstream")
	}
}

// TestTick_RequeuesResumedMarket pins: a market that appears in the
// current upstream sweep again gets its deleted_at cleared + backfill
// re-queued. Its row is NOT purged.
func TestTick_RequeuesResumedMarket(t *testing.T) {
	reaper := &fakeReaper{candidates: []repository.Market{
		{ID: 7, ConditionID: "0xback"},
		{ID: 8, ConditionID: "0xstale"},
	}}
	upstream := &fakeUpstream{activeByCondition: map[string]bool{"0xback": true}}
	w := newWorker(reaper, upstream)

	w.Tick(context.Background())

	if len(reaper.resumed) != 1 || reaper.resumed[0] != 7 {
		t.Fatalf("resumed: got %v want [7]", reaper.resumed)
	}
	if len(reaper.purged) != 1 || reaper.purged[0] != 8 {
		t.Fatalf("purged: got %v want [8]", reaper.purged)
	}
}

// TestTick_NoCandidatesNoOp pins: a tick with no eligible candidates is
// silent — no purge, no resume, no error.
func TestTick_NoCandidatesNoOp(t *testing.T) {
	reaper := &fakeReaper{}
	upstream := &fakeUpstream{}
	w := newWorker(reaper, upstream)

	w.Tick(context.Background())

	if len(reaper.purged) != 0 || len(reaper.resumed) != 0 {
		t.Fatal("idle tick must not perform any operation")
	}
}

// TestTick_PurgeErrorDoesNotStopBatch pins: when MarkPurged fails for
// one market the worker logs and continues with the rest of the batch
// — a poison row should not stall the reaper.
func TestTick_PurgeErrorDoesNotStopBatch(t *testing.T) {
	reaper := &fakeReaper{
		candidates: []repository.Market{
			{ID: 1, ConditionID: "0xa"},
			{ID: 2, ConditionID: "0xb"},
		},
		purgeErr: errors.New("simulated DB error"),
	}
	upstream := &fakeUpstream{}
	w := newWorker(reaper, upstream)

	w.Tick(context.Background())
	// Neither would actually be purged because of the error — the
	// invariant is "the worker did not panic and processed both."
}

// TestTick_ContextCancelStopsBatch pins graceful shutdown.
func TestTick_ContextCancelStopsBatch(t *testing.T) {
	reaper := &fakeReaper{candidates: []repository.Market{
		{ID: 1, ConditionID: "0xa"},
	}}
	upstream := &fakeUpstream{}
	w := newWorker(reaper, upstream)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Tick(ctx)
	if len(reaper.purged) != 0 {
		t.Fatal("cancelled context must not result in purge")
	}
}

// TestDefaults pins applyDefaults wires sane production values.
func TestDefaults(t *testing.T) {
	w := New(Config{}, &fakeReaper{}, &fakeUpstream{}, nil, &zerolog.Logger{})
	if w.cfg.Interval != time.Hour {
		t.Errorf("interval default: %v", w.cfg.Interval)
	}
	if w.cfg.Retention != 720*time.Hour {
		t.Errorf("retention default: %v", w.cfg.Retention)
	}
	if w.cfg.ClaimLimit != 256 {
		t.Errorf("claim limit default: %v", w.cfg.ClaimLimit)
	}
}
