package predictionarchival

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeStore struct {
	mu            sync.Mutex
	terminal      []repository.ArchivalCandidate
	stale         []repository.ArchivalCandidate
	archived      []int64
	staled        []int64
	archiveReason map[int64]string
	staleReason   map[int64]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{archiveReason: map[int64]string{}, staleReason: map[int64]string{}}
}

func (f *fakeStore) ListPredictionsForArchival(_ context.Context, _ time.Time, _ int32) ([]repository.ArchivalCandidate, error) {
	return f.terminal, nil
}

func (f *fakeStore) ListPredictionsForStaleSignal(_ context.Context, _ time.Time, _ int32) ([]repository.ArchivalCandidate, error) {
	return f.stale, nil
}

func (f *fakeStore) ArchivePrediction(_ context.Context, id int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived = append(f.archived, id)
	f.archiveReason[id] = reason
	return nil
}

func (f *fakeStore) MarkPredictionStaleNoSignal(_ context.Context, id int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staled = append(f.staled, id)
	f.staleReason[id] = reason
	return nil
}

// TestTick_ArchivesTerminalCandidates pins the load-bearing sweep:
// terminal-state predictions older than TerminalRetention get
// archived with a reason that includes the terminal state.
func TestTick_ArchivesTerminalCandidates(t *testing.T) {
	store := newFakeStore()
	store.terminal = []repository.ArchivalCandidate{
		{ID: 1, CurrentState: "resolved", UpdatedAt: time.Now().Add(-96 * time.Hour)},
		{ID: 2, CurrentState: "stale", UpdatedAt: time.Now().Add(-80 * time.Hour)},
	}
	w := New(Config{
		Enabled:           true,
		TerminalRetention: 72 * time.Hour,
	}, store, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Archived != 2 {
		t.Errorf("archived: got %d want 2", sum.Archived)
	}
	if r := store.archiveReason[1]; r != "terminal_retention_resolved" {
		t.Errorf("reason 1: got %q", r)
	}
	if r := store.archiveReason[2]; r != "terminal_retention_stale" {
		t.Errorf("reason 2: got %q", r)
	}
}

// TestTick_FlipsStaleNoSignal pins the second sweep: active rows
// without recent activity get state=stale.
func TestTick_FlipsStaleNoSignal(t *testing.T) {
	store := newFakeStore()
	store.stale = []repository.ArchivalCandidate{
		{ID: 7, CurrentState: "watching", UpdatedAt: time.Now().Add(-24 * time.Hour)},
	}
	w := New(Config{
		Enabled:            true,
		StaleNoSignalAfter: 18 * time.Hour,
	}, store, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Staled != 1 {
		t.Errorf("staled: got %d want 1", sum.Staled)
	}
	if !contains(store.staleReason[7], "no_signal_for") {
		t.Errorf("stale reason: %q", store.staleReason[7])
	}
}

// TestTick_EmptyCandidatesIsNoop pins zero-work behavior.
func TestTick_EmptyCandidatesIsNoop(t *testing.T) {
	store := newFakeStore()
	w := New(Config{Enabled: true}, store, nil, nil)
	sum := w.Tick(context.Background())
	if sum.Archived != 0 || sum.Staled != 0 {
		t.Errorf("expected no-op; got %+v", sum)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
