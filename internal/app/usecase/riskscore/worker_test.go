package riskscore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
)

type fakeMarkets struct {
	facts []MarketFacts
	err   error
}

func (f *fakeMarkets) ListRiskScoreCandidates(_ context.Context, _ int, _ time.Duration) ([]MarketFacts, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.facts, nil
}

type fakeSink struct {
	mu   sync.Mutex
	rows []RiskRow
}

func (s *fakeSink) UpsertRiskScore(_ context.Context, r RiskRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, r)
	return nil
}

func newCfg() Config {
	return Config{
		Enabled:      true,
		Interval:     time.Minute,
		BatchSize:    50,
		ScoreVersion: 1,
		RefreshOlder: 24 * time.Hour,
	}
}

func TestTick_PersistsRisk(t *testing.T) {
	markets := &fakeMarkets{facts: []MarketFacts{
		{ConditionID: "A", Title: "Will X win after runoff and certification?"},
		{ConditionID: "B", Title: "Will Y resolve YES?"},
	}}
	sink := &fakeSink{}
	det := rulesrisk.New(rulesrisk.Config{})
	w := New(newCfg(), markets, sink, det, nil, nil)
	w.Tick(context.Background())
	if got, want := len(sink.rows), 2; got != want {
		t.Fatalf("rows: got %d want %d", got, want)
	}
	// First row should have higher ambiguity than the bland second.
	if !(sink.rows[0].AmbiguityScore > sink.rows[1].AmbiguityScore) {
		t.Fatalf("expected runoff/certification market more ambiguous: %+v", sink.rows)
	}
}

func TestTick_BailsCleanlyOnDeps(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil, nil)
	w.Tick(context.Background()) // must not panic
}

func TestTick_ListErrorIsRecorded(t *testing.T) {
	markets := &fakeMarkets{err: errors.New("db down")}
	sink := &fakeSink{}
	det := rulesrisk.New(rulesrisk.Config{})
	w := New(newCfg(), markets, sink, det, nil, nil)
	w.Tick(context.Background())
	if len(sink.rows) != 0 {
		t.Fatalf("no rows expected on list error")
	}
}
