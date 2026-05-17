// Package detect compares recent windows against baseline windows and emits
// anomaly findings when the ratio exceeds a configured multiplier.
package detect

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	anomaly2 "github.com/Borislavv/polymarket-watchtower/internal/domain/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/rs/zerolog"
)

type Config struct {
	Interval      time.Duration
	RecentWindows []time.Duration
	Rule          anomaly2.Rule
	Cooldown      time.Duration
	Clock         func() time.Time // optional; defaults to time.Now
}

// Emitter is the dependency-inverted output side: detect doesn't care whether
// findings go to logs, webhooks, or a queue.
type Emitter interface {
	Notify(ctx context.Context, f anomaly2.Finding) error
}

type Loop struct {
	cfg      Config
	engine   *aggregate.Engine
	registry *aggregate.MarketRegistry
	emit     Emitter
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time

	mu       sync.Mutex
	lastFire map[fireKey]time.Time
}

type fireKey struct {
	scope  anomaly2.Scope
	target string
	metric anomaly2.Metric
	window time.Duration
}

func New(
	cfg Config,
	eng *aggregate.Engine,
	reg *aggregate.MarketRegistry,
	emit Emitter,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
	cfg.Rule.Normalise()
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Loop{
		cfg:      cfg,
		engine:   eng,
		registry: reg,
		emit:     emit,
		metrics:  m,
		log:      log,
		now:      now,
		lastFire: make(map[fireKey]time.Time),
	}
}

// Tick is exposed for tests that want to drive a single evaluation without
// running the ticker loop.
func (l *Loop) Tick(ctx context.Context) { l.tick(ctx) }

func (l *Loop) Run(ctx context.Context) error {
	t := time.NewTicker(l.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	now := l.now()
	snapshot := l.registry.Snapshot()

	for _, m := range snapshot {
		baseline := l.engine.BaselineWindow(m.ID, longest(l.cfg.RecentWindows))
		for _, w := range l.cfg.RecentWindows {
			recent := l.engine.Window(m.ID, w)
			label := windowLabel(w)
			l.metrics.WindowTradeRate.WithLabelValues(string(m.ID), label).Set(recent.TradesPerMinute())
			l.metrics.WindowNotionalRate.WithLabelValues(string(m.ID), label).Set(recent.NotionalPerMinute())
			l.metrics.WindowAvgSize.WithLabelValues(string(m.ID), label).Set(recent.AvgSize())

			l.evaluate(ctx, now, anomaly2.ScopeMarket, string(m.ID), m.Slug,
				anomaly2.MetricTradeRate, recent.TradesPerMinute(), baseline.TradesPerMinute(),
				recent, w, m.ID, 0)
			l.evaluate(ctx, now, anomaly2.ScopeMarket, string(m.ID), m.Slug,
				anomaly2.MetricNotionalRate, recent.NotionalPerMinute(), baseline.NotionalPerMinute(),
				recent, w, m.ID, 0)
			l.evaluate(ctx, now, anomaly2.ScopeMarket, string(m.ID), m.Slug,
				anomaly2.MetricAvgSize, recent.AvgSize(), baseline.AvgSize(),
				recent, w, m.ID, 0)
		}
	}

	l.evaluateCategories(ctx, now, snapshot)
}

// catAcc accumulates per-category recent + baseline aggregates so we can fire
// category-level findings independent of per-market thresholds.
type catAcc struct {
	recent   map[time.Duration]trade.Window
	baseline trade.Window
	label    string
}

func (l *Loop) evaluateCategories(ctx context.Context, now time.Time, snapshot []market.Market) {
	cats := map[vo.CategoryID]*catAcc{}
	for _, m := range snapshot {
		for _, cat := range m.Categories {
			acc, ok := cats[cat]
			if !ok {
				acc = &catAcc{recent: map[time.Duration]trade.Window{}}
				if c, ok := l.registry.Category(cat); ok {
					acc.label = c.Label
				}
				cats[cat] = acc
			}
			b := l.engine.BaselineWindow(m.ID, longest(l.cfg.RecentWindows))
			acc.baseline = mergeWindow(acc.baseline, b)
			for _, w := range l.cfg.RecentWindows {
				r := l.engine.Window(m.ID, w)
				acc.recent[w] = mergeWindow(acc.recent[w], r)
			}
		}
	}
	for cat, acc := range cats {
		for _, w := range l.cfg.RecentWindows {
			r := acc.recent[w]
			recentTPM := r.TradesPerMinute()
			recentNPM := r.NotionalPerMinute()
			baseTPM := acc.baseline.TradesPerMinute()
			baseNPM := acc.baseline.NotionalPerMinute()
			target := "cat:" + strconv.FormatInt(int64(cat), 10)

			l.evaluate(ctx, now, anomaly2.ScopeCategory, target, acc.label,
				anomaly2.MetricTradeRate, recentTPM, baseTPM, r, w, "", cat)
			l.evaluate(ctx, now, anomaly2.ScopeCategory, target, acc.label,
				anomaly2.MetricNotionalRate, recentNPM, baseNPM, r, w, "", cat)
		}
	}
}

func mergeWindow(a, b trade.Window) trade.Window {
	if a.Start.IsZero() {
		return b
	}
	if b.Start.IsZero() {
		return a
	}
	out := trade.Window{
		Start:    a.Start,
		End:      a.End,
		Count:    a.Count + b.Count,
		Notional: a.Notional + b.Notional,
		SizeSum:  a.SizeSum + b.SizeSum,
		SizeMin:  a.SizeMin,
		SizeMax:  a.SizeMax,
	}
	if b.SizeMin < out.SizeMin {
		out.SizeMin = b.SizeMin
	}
	if b.SizeMax > out.SizeMax {
		out.SizeMax = b.SizeMax
	}
	return out
}

func (l *Loop) evaluate(
	ctx context.Context,
	now time.Time,
	scope anomaly2.Scope,
	target string,
	label string,
	metric anomaly2.Metric,
	recentVal, baseVal float64,
	recent trade.Window,
	winLen time.Duration,
	market vo.MarketID,
	cat vo.CategoryID,
) {
	ratio := safeRatio(recentVal, baseVal)
	l.metrics.AnomalyMultiplier.WithLabelValues(string(scope), string(metric)).Set(ratio)
	sev, ok := l.cfg.Rule.SeverityFor(ratio)
	if !ok {
		return
	}
	// Avg-size signals are pure rate-of-change and don't need a volume floor.
	if metric != anomaly2.MetricAvgSize {
		if recent.Notional < l.cfg.Rule.MinNotional {
			return
		}
		if recent.Count < int64(l.cfg.Rule.MinTrades) {
			return
		}
	}

	key := fireKey{scope: scope, target: target, metric: metric, window: winLen}
	l.mu.Lock()
	if last, ok := l.lastFire[key]; ok && now.Sub(last) < l.cfg.Cooldown {
		l.mu.Unlock()
		return
	}
	l.lastFire[key] = now
	l.mu.Unlock()

	f := anomaly2.Finding{
		At:          now,
		Scope:       scope,
		Market:      market,
		Category:    cat,
		Label:       label,
		MarketURL:   marketURL(scope, label),
		Metric:      metric,
		Severity:    sev,
		Multiplier:  ratio,
		Recent:      recentVal,
		Baseline:    baseVal,
		WindowLen:   winLen,
		BaselineLen: l.engine.BaselineWindowLen(),
	}
	l.metrics.Anomalies.WithLabelValues(string(scope), string(metric), string(sev)).Inc()
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit failed")
	}
}

func windowLabel(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	default:
		return d.String()
	}
}

func longest(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

// marketURL is a best-effort link to the Polymarket market page. The Polymarket
// canonical URL uses the event slug, but for single-market events the market
// slug is the event slug, which is the common case. For category scope we
// don't emit a link.
func marketURL(scope anomaly2.Scope, slug string) string {
	if scope != anomaly2.ScopeMarket || slug == "" {
		return ""
	}
	return "https://polymarket.com/event/" + slug
}

func safeRatio(num, den float64) float64 {
	if den <= 0 {
		if num > 0 {
			return 1e6 // unbounded; clamp so metrics labels and rules stay sane
		}
		return 0
	}
	r := num / den
	if r > 1e6 {
		r = 1e6
	}
	return r
}
