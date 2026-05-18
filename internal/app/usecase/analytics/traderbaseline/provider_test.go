package traderbaseline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeTraders struct {
	byWallet map[string]repository.Trader
}

func (f *fakeTraders) GetByWallet(_ context.Context, w string) (repository.Trader, error) {
	t, ok := f.byWallet[w]
	if !ok {
		return repository.Trader{}, repository.ErrTraderNotFound
	}
	return t, nil
}

type fakeStats struct {
	dist      repository.TraderDistribution
	err       error
	calls     int
	lastID    int64
	lastSince time.Time
}

func (f *fakeStats) Distribution(_ context.Context, id int64, since time.Time) (repository.TraderDistribution, error) {
	f.calls++
	f.lastID = id
	f.lastSince = since
	return f.dist, f.err
}

func TestProvider_EmptyWalletYieldsZeroStats(t *testing.T) {
	p := New(Config{}, &fakeTraders{}, &fakeStats{})
	stats, err := p.Stats(context.Background(), "")
	if err != nil {
		t.Fatalf("Stats(\"\"): %v", err)
	}
	if stats.Count != 0 {
		t.Errorf("empty wallet must yield zero stats, got %+v", stats)
	}
}

func TestProvider_UnknownWalletYieldsZeroStatsNoError(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{}}
	st := &fakeStats{}
	p := New(Config{}, tr, st)

	stats, err := p.Stats(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 0 {
		t.Errorf("unknown wallet must yield zero stats, got %+v", stats)
	}
	if st.calls != 0 {
		t.Errorf("Distribution must not be called for unknown wallet, got %d", st.calls)
	}
}

func TestProvider_PassesWindowedSinceCutoff(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 7}}}
	st := &fakeStats{dist: repository.TraderDistribution{
		SampleCount:       40,
		TotalNotionalUSD:  8_000,
		MeanNotionalUSD:   200,
		MedianNotionalUSD: 180,
		P95NotionalUSD:    600,
		OldestAt:          now.Add(-60 * 24 * time.Hour),
		NewestAt:          now.Add(-1 * time.Hour),
	}}
	p := New(Config{Window: 90 * 24 * time.Hour, Clock: func() time.Time { return now }}, tr, st)

	stats, err := p.Stats(context.Background(), "0xa")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 40 || stats.MedianUSD != 180 {
		t.Fatalf("stats: %+v", stats)
	}
	wantSince := now.Add(-90 * 24 * time.Hour)
	if !st.lastSince.Equal(wantSince) {
		t.Errorf("since: got %s want %s", st.lastSince, wantSince)
	}
	if st.lastID != 7 {
		t.Errorf("trader id: got %d want 7", st.lastID)
	}
}

func TestProvider_ZeroWindowLiftsLowerBound(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	st := &fakeStats{dist: repository.TraderDistribution{SampleCount: 1}}
	p := New(Config{Window: 0}, tr, st)

	if _, err := p.Stats(context.Background(), "0xa"); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !st.lastSince.IsZero() {
		t.Errorf("Window=0 must pass zero time, got %s", st.lastSince)
	}
}

func TestProvider_EmptyDistributionPropagatesAsZero(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	st := &fakeStats{dist: repository.TraderDistribution{}}
	p := New(Config{}, tr, st)

	stats, err := p.Stats(context.Background(), "0xa")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Count != 0 {
		t.Errorf("empty distribution must yield zero stats, got %+v", stats)
	}
}

func TestProvider_CachesIDLookup(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	st := &fakeStats{dist: repository.TraderDistribution{SampleCount: 1}}
	p := New(Config{}, tr, st)

	for i := 0; i < 5; i++ {
		if _, err := p.Stats(context.Background(), "0xa"); err != nil {
			t.Fatalf("Stats: %v", err)
		}
	}
	p.Forget("0xa")
	if _, err := p.Stats(context.Background(), "0xa"); err != nil {
		t.Fatalf("after Forget: %v", err)
	}
}

func TestProvider_RepositoryErrorPropagates(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	st := &fakeStats{err: errors.New("boom")}
	p := New(Config{}, tr, st)

	_, err := p.Stats(context.Background(), "0xa")
	if err == nil {
		t.Fatal("expected error from repo")
	}
}
