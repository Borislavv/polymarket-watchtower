package marketlinks

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeEvents struct {
	hints []LinkHint
	err   error
	calls int
}

func (f *fakeEvents) ListLinkHints(_ context.Context, _ int) ([]LinkHint, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.hints, nil
}

type fakeSink struct {
	edges   []Edge
	failOn  string
	upserts int
}

func (s *fakeSink) UpsertMarketLink(_ context.Context, e Edge) error {
	s.upserts++
	if s.failOn != "" && e.DstConditionID == s.failOn {
		return errors.New("synthetic upsert failure")
	}
	s.edges = append(s.edges, e)
	return nil
}

func newCfg() Config {
	return Config{
		Enabled:        true,
		Interval:       time.Minute,
		BatchSize:      10,
		LinkVersion:    1,
		IncludeOpposed: true,
		MinConfidence:  0.3,
	}
}

func TestTick_PersistsEdgesAboveMinConfidence(t *testing.T) {
	ev := &fakeEvents{hints: []LinkHint{{
		EventSlug:         "ny-mayor",
		SourceConditionID: "cond-A",
		Targets: []LinkTarget{
			{ConditionID: "cond-B", LinkType: "same_event", Direction: "aligned", Confidence: 0.9},
			{ConditionID: "cond-C", LinkType: "same_event", Direction: "opposed", Confidence: 0.8},
			{ConditionID: "cond-D", LinkType: "same_tag", Direction: "unknown", Confidence: 0.1}, // below floor
		},
	}}}
	sink := &fakeSink{}
	b := New(newCfg(), ev, sink, nil, nil)
	b.Tick(context.Background())
	if got, want := len(sink.edges), 2; got != want {
		t.Fatalf("edges persisted: got %d want %d (%+v)", got, want, sink.edges)
	}
}

func TestTick_NoOpWhenDisabledOrDepsMissing(t *testing.T) {
	cfg := newCfg()
	cfg.Enabled = true
	b := New(cfg, nil, nil, nil, nil)
	b.Tick(context.Background()) // must not panic
}

func TestTick_ListerErrorDoesNotCrash(t *testing.T) {
	ev := &fakeEvents{err: errors.New("db down")}
	sink := &fakeSink{}
	b := New(newCfg(), ev, sink, nil, nil)
	b.Tick(context.Background())
	if sink.upserts != 0 {
		t.Fatalf("must not upsert on lister error; got %d", sink.upserts)
	}
}

func TestTick_OpposedSkippedWhenDisabled(t *testing.T) {
	cfg := newCfg()
	cfg.IncludeOpposed = false
	ev := &fakeEvents{hints: []LinkHint{{
		SourceConditionID: "cond-A",
		Targets: []LinkTarget{
			{ConditionID: "cond-B", LinkType: "same_event", Direction: "aligned", Confidence: 0.9},
			{ConditionID: "cond-C", LinkType: "same_event", Direction: "opposed", Confidence: 0.9},
		},
	}}}
	sink := &fakeSink{}
	b := New(cfg, ev, sink, nil, nil)
	b.Tick(context.Background())
	if got, want := len(sink.edges), 1; got != want {
		t.Fatalf("opposed must be skipped; got %d edges", got)
	}
}

func TestTick_SelfLoopSkipped(t *testing.T) {
	ev := &fakeEvents{hints: []LinkHint{{
		SourceConditionID: "cond-A",
		Targets: []LinkTarget{
			{ConditionID: "cond-A", LinkType: "same_event", Direction: "aligned", Confidence: 0.9},
			{ConditionID: "cond-B", LinkType: "same_event", Direction: "aligned", Confidence: 0.9},
		},
	}}}
	sink := &fakeSink{}
	b := New(newCfg(), ev, sink, nil, nil)
	b.Tick(context.Background())
	if got, want := len(sink.edges), 1; got != want {
		t.Fatalf("self-loop must be skipped; got %d edges", got)
	}
	if sink.edges[0].DstConditionID != "cond-B" {
		t.Fatalf("expected cond-B; got %s", sink.edges[0].DstConditionID)
	}
}
