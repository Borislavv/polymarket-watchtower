package detect

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/score"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/rs/zerolog"
)

// VolumeConfig configures the legacy aggregate-volume detector.
type VolumeConfig struct {
	Interval      time.Duration
	RecentWindows []time.Duration
	Multipliers   []float64
	MinNotional   float64
	MinTrades     int
	Cooldown      time.Duration
	Filter        *category.Filter // nil => allow all
	Clock         func() time.Time
}

// VolumeLoop emits findings when (recent-window rate / baseline rate) for
// trade_rate, notional_rate, or avg_size crosses a multiplier ladder. It is
// the explicit "volume" mode — alerts come from per-market and per-category
// aggregate ratios rather than individual trades.
//
// VolumeLoop implements collect.TradeObserver with a no-op Observe so it can
// be wired into collect interchangeably with the single_cluster detector.
type VolumeLoop struct {
	cfg      VolumeConfig
	engine   *aggregate.Engine
	registry *aggregate.MarketRegistry
	emit     Emitter
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time

	mu       sync.Mutex
	lastFire map[volumeKey]time.Time
}

type volumeKey struct {
	scope, target, metric string
	window                time.Duration
}

// NewVolume wires the volume detector.
func NewVolume(
	cfg VolumeConfig,
	eng *aggregate.Engine,
	reg *aggregate.MarketRegistry,
	emit Emitter,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *VolumeLoop {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Minute
	}
	if cfg.Filter == nil {
		cfg.Filter = category.NewFilter(nil)
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &VolumeLoop{
		cfg:      cfg,
		engine:   eng,
		registry: reg,
		emit:     emit,
		metrics:  m,
		log:      log,
		now:      now,
		lastFire: make(map[volumeKey]time.Time),
	}
}

// Observe is a no-op — volume mode reads from the aggregate engine on its own
// ticker, not on the per-trade hot path.
func (l *VolumeLoop) Observe(_ context.Context, _ market.Market, _ trade.Trade) {}

// Run ticks every Interval until ctx is cancelled.
func (l *VolumeLoop) Run(ctx context.Context) error {
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

// Tick is exposed for deterministic tests.
func (l *VolumeLoop) Tick(ctx context.Context) { l.tick(ctx) }

func (l *VolumeLoop) tick(ctx context.Context) {
	now := l.now()
	longest := longestWindow(l.cfg.RecentWindows)
	for _, m := range l.registry.Snapshot() {
		baseline := l.engine.BaselineWindow(m.ID, longest)
		for _, w := range l.cfg.RecentWindows {
			recent := l.engine.Window(m.ID, w)
			l.evalMetric(ctx, now, m, baseline, recent, w, "trade_rate", recent.TradesPerMinute(), baseline.TradesPerMinute())
			l.evalMetric(ctx, now, m, baseline, recent, w, "notional_rate", recent.NotionalPerMinute(), baseline.NotionalPerMinute())
			l.evalMetric(ctx, now, m, baseline, recent, w, "avg_size", recent.AvgSize(), baseline.AvgSize())
		}
	}
}

func (l *VolumeLoop) evalMetric(
	ctx context.Context,
	now time.Time,
	m market.Market,
	baseline, recent trade.Window,
	winLen time.Duration,
	metric string,
	recentVal, baseVal float64,
) {
	ratio := safeRatio(recentVal, baseVal)
	sev := volumeSeverity(ratio, l.cfg.Multipliers)
	if sev == "" {
		return
	}
	if metric != "avg_size" {
		if recent.Notional < l.cfg.MinNotional || recent.Count < int64(l.cfg.MinTrades) {
			return
		}
	}
	key := volumeKey{scope: "market", target: string(m.ID), metric: metric, window: winLen}
	l.mu.Lock()
	if last, ok := l.lastFire[key]; ok && now.Sub(last) < l.cfg.Cooldown {
		l.mu.Unlock()
		return
	}
	l.lastFire[key] = now
	l.mu.Unlock()

	catRef := anomaly.CategoryRef{}
	if len(m.Categories) > 0 {
		if c, ok := l.registry.Category(m.Categories[0]); ok {
			if !l.cfg.Filter.Allowed(c.Slug, c.Label) {
				l.metrics.CategoryFilterSkipped.WithLabelValues("detect").Inc()
				return
			}
			catRef = anomaly.CategoryRef{ID: c.ID, Slug: c.Slug, Label: c.Label}
		}
	}
	f := anomaly.Finding{
		Kind:     anomaly.KindTradeAnomaly,
		Severity: sev,
		At:       now,
		Reason:   "volume:" + metric,
		Trade: &anomaly.TradeRef{
			Market:      m.ID,
			Slug:        m.Slug,
			Question:    m.Question,
			NotionalUSD: recent.Notional,
			At:          now,
		},
		Category:            &catRef,
		MarketMultiplier:    ratio,
		EffectiveMultiplier: ratio,
		MultiplierAxis:      string(score.MultiplierAxisMarket),
	}
	l.metrics.TradeAnomalies.WithLabelValues(string(sev), categoryLabelOrDefault(catRef), "volume:"+metric).Inc()
	if err := l.emit.Notify(ctx, f); err != nil && l.log != nil {
		l.log.Err(err).Msg("volume: emit failed")
	}
}

func longestWindow(ws []time.Duration) time.Duration {
	var m time.Duration
	for _, w := range ws {
		if w > m {
			m = w
		}
	}
	return m
}

// volumeSeverity is a small 3-rung mapper local to the legacy volume detector;
// the single_cluster path has its own thresholds in anomaly.Thresholds.
func volumeSeverity(v float64, ladder []float64) anomaly.Severity {
	if len(ladder) == 0 || v < ladder[0] {
		return ""
	}
	top := ladder[len(ladder)-1]
	mid := top
	if len(ladder) >= 2 {
		mid = ladder[len(ladder)-2]
	}
	switch {
	case v >= top && len(ladder) >= 3:
		return anomaly.SeverityCritical
	case v >= mid && len(ladder) >= 2:
		return anomaly.SeverityWarning
	default:
		return anomaly.SeverityInfo
	}
}

func safeRatio(num, den float64) float64 {
	if den <= 0 {
		if num > 0 {
			return 1e6
		}
		return 0
	}
	if r := num / den; r < 1e6 {
		return r
	}
	return 1e6
}

func categoryLabelOrDefault(c anomaly.CategoryRef) string {
	if c.Label != "" {
		return c.Label
	}
	return "uncategorised"
}

// avoid an unused-import warning for strconv during dev iterations.
var _ = strconv.Itoa
var _ vo.CategoryID
