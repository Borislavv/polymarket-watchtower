package detect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	anomaly2 "github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/rs/zerolog"
)

type capturingEmitter struct {
	mu       sync.Mutex
	findings []anomaly2.Finding
}

func (e *capturingEmitter) Notify(_ context.Context, f anomaly2.Finding) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.findings = append(e.findings, f)
	return nil
}
func (e *capturingEmitter) all() []anomaly2.Finding {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]anomaly2.Finding, len(e.findings))
	copy(cp, e.findings)
	return cp
}

// scenarioEngine builds an engine pre-seeded with N baseline trades evenly
// spread over `baselineSpan` and M recent trades inside the last `recentSpan`.
func scenarioEngine(t *testing.T, now time.Time, mid vo.MarketID,
	baselineN int, baselineSpan time.Duration,
	recentN int, recentSpan time.Duration,
	recentSize float64) *aggregate.Engine {
	t.Helper()
	e := aggregate.New(aggregate.Config{
		Bucket:   time.Minute,
		Baseline: baselineSpan + recentSpan + time.Minute,
		Clock:    func() time.Time { return now },
	})
	// baseline trades sit in [now-baseline-recent, now-recent)
	if baselineN > 0 {
		baseStart := now.Add(-baselineSpan - recentSpan)
		step := baselineSpan / time.Duration(baselineN)
		for i := 0; i < baselineN; i++ {
			ts := baseStart.Add(time.Duration(i) * step)
			e.Ingest(trade.Trade{Market: mid, Timestamp: ts, Size: 1, Price: 0.5})
		}
	}
	// recent trades sit in [now-recent, now)
	if recentN > 0 {
		step := recentSpan / time.Duration(recentN)
		for i := 0; i < recentN; i++ {
			ts := now.Add(-recentSpan).Add(time.Duration(i) * step)
			e.Ingest(trade.Trade{Market: mid, Timestamp: ts, Size: recentSize, Price: 0.5})
		}
	}
	return e
}

func newDetectFixture(t *testing.T, now time.Time, eng *aggregate.Engine, mult []float64, minNotional float64, minTrades int) (*Loop, *aggregate.MarketRegistry, *capturingEmitter) {
	t.Helper()
	reg := aggregate.NewRegistry()
	reg.Replace([]market.Market{{ID: "0xa", Slug: "spike-market", Active: true}}, nil)
	emit := &capturingEmitter{}
	log := zerolog.Nop()
	loop := New(Config{
		Interval:      time.Minute,
		RecentWindows: []time.Duration{time.Hour},
		Rule:          anomaly2.Rule{Multipliers: mult, MinNotional: minNotional, MinTrades: minTrades},
		Cooldown:      time.Hour,
		Clock:         func() time.Time { return now },
	}, eng, reg, emit, metrics.New(), &log)
	return loop, reg, emit
}

func TestDetectNoFireOnFlatActivity(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	eng := scenarioEngine(t, now, "0xa", 60, time.Hour, 1, time.Hour, 1) // 1 recent vs 60 baseline
	loop, _, emit := newDetectFixture(t, now, eng, []float64{30, 100, 1000}, 0, 0)
	loop.Tick(context.Background())
	if got := emit.all(); len(got) != 0 {
		t.Fatalf("expected 0 findings, got %d: %+v", len(got), got)
	}
}

func TestDetectFiresOnX30Spike(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// Baseline: 10 trades over the hour ~= 0.16/min.
	// Recent:  300 trades over the hour = 5/min  → ratio ~30x.
	eng := scenarioEngine(t, now, "0xa", 10, time.Hour, 300, time.Hour, 10)
	loop, _, emit := newDetectFixture(t, now, eng, []float64{30, 100, 1000}, 0, 0)
	loop.Tick(context.Background())
	findings := emit.all()
	if len(findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	var sawWarn bool
	for _, f := range findings {
		if f.Severity == anomaly2.SeverityWarn {
			sawWarn = true
		}
		if f.MarketURL != "https://polymarket.com/event/spike-market" {
			t.Errorf("market URL: %q", f.MarketURL)
		}
	}
	if !sawWarn {
		t.Errorf("expected warn severity in %+v", findings)
	}
}

func TestDetectFiresFatalOnX1000Spike(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// baseline 1 trade total over the hour; recent 1500 trades.
	eng := scenarioEngine(t, now, "0xa", 1, time.Hour, 1500, time.Hour, 10)
	loop, _, emit := newDetectFixture(t, now, eng, []float64{30, 100, 1000}, 0, 0)
	loop.Tick(context.Background())
	var sawFatal bool
	for _, f := range emit.all() {
		if f.Severity == anomaly2.SeverityFatal {
			sawFatal = true
		}
	}
	if !sawFatal {
		t.Fatal("expected fatal severity for x1000 spike")
	}
}

func TestDetectVolumeFloorBlocksFire(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	// ratio is huge but recent notional is tiny.
	eng := scenarioEngine(t, now, "0xa", 1, time.Hour, 100, time.Hour, 0.01)
	loop, _, emit := newDetectFixture(t, now, eng, []float64{30}, 1_000_000, 0)
	loop.Tick(context.Background())
	for _, f := range emit.all() {
		if f.Metric != anomaly2.MetricAvgSize {
			t.Errorf("expected only avg-size findings under floor, got %+v", f)
		}
	}
}

func TestDetectCooldownSuppressesRepeat(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	eng := scenarioEngine(t, now, "0xa", 1, time.Hour, 1500, time.Hour, 10)
	loop, _, emit := newDetectFixture(t, now, eng, []float64{30}, 0, 0)
	loop.Tick(context.Background())
	first := len(emit.all())
	loop.Tick(context.Background())
	if len(emit.all()) != first {
		t.Fatalf("cooldown should suppress repeats: first=%d second=%d", first, len(emit.all()))
	}
}
