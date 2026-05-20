package aibudget

import (
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestAllow_UnderCap covers the canonical pass-through: a small
// estimated cost goes through when both per-bucket and global caps
// have headroom, and the subsequent Charge updates the gauge.
func TestAllow_UnderCap(t *testing.T) {
	met := metrics.New()
	m := New(Config{
		GlobalDailyBudgetUSD:  25,
		BucketDailyBudgetsUSD: map[string]float64{BucketCatalystImporter: 8},
		Clock:                 fixedClock(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)),
	}, met)
	ok, reason := m.Allow(BucketCatalystImporter, 0.05)
	if !ok || reason != "" {
		t.Fatalf("expected allow, got (%v,%q)", ok, reason)
	}
	m.Charge(BucketCatalystImporter, 0.04)
	g, per := m.Snapshot()
	if g != 0.04 {
		t.Errorf("global: got %v want 0.04", g)
	}
	if per[BucketCatalystImporter] != 0.04 {
		t.Errorf("bucket: got %v want 0.04", per[BucketCatalystImporter])
	}
}

// TestAllow_BucketExhausted exercises the per-bucket denial path
// while global has headroom — the load-bearing case for priority.
// (Low-priority buckets stop on their own bucket cap first.)
func TestAllow_BucketExhausted(t *testing.T) {
	m := New(Config{
		GlobalDailyBudgetUSD:  25,
		BucketDailyBudgetsUSD: map[string]float64{BucketCatalystImporter: 1},
		Clock:                 fixedClock(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)),
	}, nil)
	for i := 0; i < 5; i++ {
		ok, _ := m.Allow(BucketCatalystImporter, 0.30)
		if !ok {
			break
		}
		m.Charge(BucketCatalystImporter, 0.30)
	}
	ok, reason := m.Allow(BucketCatalystImporter, 0.30)
	if ok {
		t.Errorf("expected bucket_exhausted denial")
	}
	if reason != "bucket_exhausted" {
		t.Errorf("reason: got %q want bucket_exhausted", reason)
	}
	// Higher-priority bucket with no per-bucket cap still passes
	// because only this bucket's cap was hit.
	ok2, _ := m.Allow(BucketAlertAnalysis, 0.10)
	if !ok2 {
		t.Errorf("alert_analysis must not be denied when only catalyst bucket is exhausted")
	}
}

// TestAllow_GlobalExhausted exercises the global denial path.
func TestAllow_GlobalExhausted(t *testing.T) {
	m := New(Config{
		GlobalDailyBudgetUSD: 1.0,
		Clock:                fixedClock(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)),
	}, nil)
	ok, _ := m.Allow(BucketAlertAnalysis, 0.8)
	if !ok {
		t.Fatal("first call should pass")
	}
	m.Charge(BucketAlertAnalysis, 0.8)
	ok, reason := m.Allow(BucketPredictionEvolve, 0.5)
	if ok {
		t.Fatalf("expected global denial")
	}
	if reason != "global_exhausted" {
		t.Errorf("reason: got %q want global_exhausted", reason)
	}
}

// TestAllow_ZeroCapMeansUncapped pins the fail-open contract for
// tests / one-shot dry runs: a missing bucket cap or 0 means uncapped.
func TestAllow_ZeroCapMeansUncapped(t *testing.T) {
	m := New(Config{
		GlobalDailyBudgetUSD:  0,
		BucketDailyBudgetsUSD: map[string]float64{},
		Clock:                 fixedClock(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)),
	}, nil)
	for i := 0; i < 1000; i++ {
		ok, _ := m.Allow(BucketCatalystImporter, 1.0)
		if !ok {
			t.Fatal("expected uncapped pass")
		}
		m.Charge(BucketCatalystImporter, 1.0)
	}
}

// TestDailyRolloverResets covers the UTC-midnight reset: yesterday's
// exhausted bucket is fresh today.
func TestDailyRolloverResets(t *testing.T) {
	cur := time.Date(2026, 1, 1, 23, 59, 0, 0, time.UTC)
	clk := &mutableClock{t: cur}
	m := New(Config{
		GlobalDailyBudgetUSD:  10,
		BucketDailyBudgetsUSD: map[string]float64{BucketCatalystImporter: 1},
		Clock:                 clk.Now,
	}, nil)
	ok, _ := m.Allow(BucketCatalystImporter, 0.9)
	if !ok {
		t.Fatal("first call should pass")
	}
	m.Charge(BucketCatalystImporter, 0.9)
	ok, _ = m.Allow(BucketCatalystImporter, 0.2)
	if ok {
		t.Fatal("expected bucket exhausted before rollover")
	}
	clk.set(time.Date(2026, 1, 2, 0, 1, 0, 0, time.UTC))
	ok, _ = m.Allow(BucketCatalystImporter, 0.2)
	if !ok {
		t.Fatal("expected fresh budget after rollover")
	}
	g, _ := m.Snapshot()
	if g != 0 {
		t.Errorf("global after rollover: got %v want 0", g)
	}
}

// TestAllow_FailOpenOnNilManager covers the worker-side defensive
// check: a worker holding a nil *Manager treats every call as allowed.
// This is what enables a deployment to disable the budget governor
// by passing nil rather than maintaining a separate flag.
func TestAllow_FailOpenOnNilManager(t *testing.T) {
	var m *Manager
	ok, _ := m.Allow(BucketAlertAnalysis, 1000)
	if !ok {
		t.Fatal("nil manager must fail-open")
	}
	m.Charge(BucketAlertAnalysis, 1000) // no panic
}

// TestConcurrentCharge stresses the mutex path with many goroutines
// hammering Allow + Charge — pins the absence of races (run with
// -race) and the correctness of the running total.
func TestConcurrentCharge(t *testing.T) {
	m := New(Config{
		GlobalDailyBudgetUSD: 0,
		Clock:                fixedClock(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)),
	}, nil)
	const goroutines = 16
	const callsPerG = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerG; j++ {
				if ok, _ := m.Allow(BucketCatalystImporter, 0.01); ok {
					m.Charge(BucketCatalystImporter, 0.01)
				}
			}
		}()
	}
	wg.Wait()
	g, per := m.Snapshot()
	wantTotal := float64(goroutines*callsPerG) * 0.01
	if !approxEqual(g, wantTotal, 1e-6) {
		t.Errorf("global: got %v want %v", g, wantTotal)
	}
	if !approxEqual(per[BucketCatalystImporter], wantTotal, 1e-6) {
		t.Errorf("bucket: got %v want %v", per[BucketCatalystImporter], wantTotal)
	}
}

type mutableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mutableClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func approxEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
