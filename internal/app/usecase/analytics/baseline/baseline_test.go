package baseline

import (
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

func k(cat int64) Key {
	return Key{Category: vo.CategoryID(cat), Market: "m", OutcomeToken: "t"}
}

func TestEmptyBucket(t *testing.T) {
	b := New(Config{Window: time.Hour})
	if got := b.Stats(k(1)); got.Count != 0 || got.MeanUSD != 0 || got.MedianUSD != 0 {
		t.Fatalf("expected zero stats, got %+v", got)
	}
}

func TestAddAndStatsBasic(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := New(Config{Window: time.Hour, Clock: func() time.Time { return now }})
	for _, v := range []float64{10, 20, 30, 40, 50} {
		b.Add(k(1), v, now.Add(-time.Minute))
	}
	s := b.Stats(k(1))
	if s.Count != 5 {
		t.Fatalf("count: %d", s.Count)
	}
	if s.MeanUSD != 30 {
		t.Fatalf("mean: %v", s.MeanUSD)
	}
	if s.MedianUSD != 30 {
		t.Fatalf("median: %v", s.MedianUSD)
	}
	if s.P95USD != 50 {
		t.Fatalf("p95: %v", s.P95USD)
	}
	if s.TotalUSD != 150 {
		t.Fatalf("total: %v", s.TotalUSD)
	}
}

func TestDropsSamplesOlderThanWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := New(Config{Window: time.Hour, Clock: func() time.Time { return now }})
	b.Add(k(1), 10, now.Add(-2*time.Hour)) // outside window
	b.Add(k(1), 20, now.Add(-time.Minute)) // inside
	s := b.Stats(k(1))
	if s.Count != 1 || s.MeanUSD != 20 {
		t.Fatalf("expected 1 fresh sample, got %+v", s)
	}
}

func TestRingCapEvictsOldest(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := New(Config{Window: time.Hour, MaxSamples: 3, Clock: func() time.Time { return now }})
	for _, v := range []float64{1, 2, 3, 4, 5} {
		b.Add(k(1), v, now)
	}
	s := b.Stats(k(1))
	if s.Count != 3 {
		t.Fatalf("count: %d (expected 3, cap=3)", s.Count)
	}
	// Should retain the last 3 (3,4,5). Sum = 12.
	if s.TotalUSD != 12 {
		t.Fatalf("total: %v", s.TotalUSD)
	}
}

func TestNonPositiveNotionalsIgnored(t *testing.T) {
	b := New(Config{Window: time.Hour})
	b.Add(k(1), -5, time.Now())
	b.Add(k(1), 0, time.Now())
	if got := b.Stats(k(1)); got.Count != 0 {
		t.Fatalf("expected no samples, got %+v", got)
	}
}

func TestBucketsIsolated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := New(Config{Window: time.Hour, Clock: func() time.Time { return now }})
	b.Add(k(1), 100, now)
	b.Add(k(2), 1, now)
	if got := b.Stats(k(1)).MeanUSD; got != 100 {
		t.Fatalf("bucket 1 mean: %v", got)
	}
	if got := b.Stats(k(2)).MeanUSD; got != 1 {
		t.Fatalf("bucket 2 mean: %v", got)
	}
}

func TestConcurrentAddRaceSafe(t *testing.T) {
	b := New(Config{Window: time.Hour})
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				b.Add(k(1), 10, now)
			}
		}()
	}
	wg.Wait()
	if got := b.Stats(k(1)).Count; got == 0 {
		t.Fatal("no samples after concurrent adds")
	}
}
