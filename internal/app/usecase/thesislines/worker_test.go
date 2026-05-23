package thesislines

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeLister struct {
	rows []Aggregate
	err  error
}

func (f *fakeLister) AggregateWalletThesisLines(_ context.Context, _ time.Time, _ int) ([]Aggregate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeSink struct {
	mu    sync.Mutex
	rows  []Aggregate
	hours int
}

func (s *fakeSink) UpsertWalletThesisLine(_ context.Context, a Aggregate, h int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, a)
	s.hours = h
	return nil
}

func newCfg() Config {
	return Config{Enabled: true, Interval: 10 * time.Minute, Lookback: 30 * 24 * time.Hour, BatchSize: 100}
}

func TestTick_PersistsAggregates(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	l := &fakeLister{rows: []Aggregate{
		{Wallet: "0x1", ConditionID: "A", EventSlug: "ev1", Side: "BUY", NotionalUSD: 5000, Trades: 4, LastTradedAt: now},
		{Wallet: "0x2", ConditionID: "B", EventSlug: "ev1", Side: "SELL", NotionalUSD: 2500, Trades: 3, LastTradedAt: now},
	}}
	s := &fakeSink{}
	w := New(newCfg(), l, s, nil, nil).WithClock(func() time.Time { return now })
	w.Tick(context.Background())
	if got := len(s.rows); got != 2 {
		t.Fatalf("expected 2 upserts; got %d", got)
	}
	if s.hours != int((30 * 24 * time.Hour).Hours()) {
		t.Fatalf("expected lookbackHours=720; got %d", s.hours)
	}
}

func TestTick_EmptyDoesNotPanic(t *testing.T) {
	w := New(newCfg(), &fakeLister{}, &fakeSink{}, nil, nil)
	w.Tick(context.Background())
}

func TestTick_ErrorBailsCleanly(t *testing.T) {
	w := New(newCfg(), &fakeLister{err: errors.New("db down")}, &fakeSink{}, nil, nil)
	w.Tick(context.Background())
}

func TestTick_DepsMissingNoOp(t *testing.T) {
	w := New(newCfg(), nil, nil, nil, nil)
	w.Tick(context.Background())
}

func TestTick_SkipsEmptyWalletOrCondition(t *testing.T) {
	now := time.Now()
	l := &fakeLister{rows: []Aggregate{
		{Wallet: "", ConditionID: "A", Side: "BUY", LastTradedAt: now},
		{Wallet: "0x1", ConditionID: "", Side: "BUY", LastTradedAt: now},
		{Wallet: "0x1", ConditionID: "A", Side: "BUY", NotionalUSD: 100, Trades: 1, LastTradedAt: now},
	}}
	s := &fakeSink{}
	w := New(newCfg(), l, s, nil, nil)
	w.Tick(context.Background())
	if got := len(s.rows); got != 1 {
		t.Fatalf("expected exactly 1 valid row; got %d", got)
	}
}
