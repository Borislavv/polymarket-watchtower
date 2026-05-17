// Package detect is the per-trade anomaly pipeline. It is called synchronously
// from the collect loop for every freshly ingested trade and is responsible
// for:
//
//  1. Updating the per-(category, market, outcome) baseline of trade
//     notionals (for use by the next scoring round).
//  2. Scoring the trade against multiplier and absolute USD ladders.
//  3. Observing anomalous trades in a per-category cluster detector that
//     emits a HARD "CategoryWatchRequired" alert when many sharks circle one
//     category at once.
//  4. Periodically refreshing aggregate Grafana gauges (trade_rate /
//     notional_rate / avg_size) — kept as supporting telemetry only;
//     **never** drives alerts on its own.
//
// All state mutation is concurrency-safe; the package is safe to call from
// multiple collect goroutines simultaneously.
package detect

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/score"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/rs/zerolog"
)

// Emitter receives findings. Decoupled so detect doesn't care whether output is
// logs, telegram, webhooks, or all of them.
type Emitter interface {
	Notify(ctx context.Context, f anomaly.Finding) error
}

// Config wires the detector. Defaults fill in for zero-valued fields.
type Config struct {
	Thresholds     anomaly.Thresholds
	Baseline       baseline.Config
	Cluster        cluster.Config
	Filter         *category.Filter // nil => allow all (no filtering)
	RecentWindows  []time.Duration
	GaugeInterval  time.Duration // how often Run() refreshes supporting gauges
	PolymarketBase string        // "https://polymarket.com" (no trailing slash)
	GrafanaBase    string        // public base URL for Grafana deep-links; "" disables
	GrafanaDashUID string        // dashboard UID for deep-link
	GrafanaContext time.Duration // ±window around trade time in Grafana link
	// Lifecycle gating: alerts only fire when the market is at or past
	// LifecycleAlertFromPct of its lifetime, and are marked Hot when at or
	// past LifecycleHotFromPct. Markets without start/end dates bypass the
	// gate.
	LifecycleAlertFromPct float64
	LifecycleHotFromPct   float64
	// MarketMinAge gates alerts on absolute market age (now - StartDate).
	// 0 disables. Markets without StartDate bypass.
	MarketMinAge time.Duration
	// BaselineMinReadySpan requires the observed baseline span (newest minus
	// oldest sample) to clear this floor before alerts can fire. 0 disables.
	// Distinct from BaselineWindow, which is the *maximum* lookback.
	BaselineMinReadySpan time.Duration
	// AllowUnknownMarketLifecycle: when false (default), markets without
	// StartDate/EndDate are silently skipped — fail-closed. Set true to
	// allow them through (legacy / debugging).
	AllowUnknownMarketLifecycle bool
	// SportsKeywords is consulted against the market title, event slug, and
	// market slug as a secondary blacklist. Catches sports markets attached
	// to non-sports categories like Polymarket's "Hide From New".
	SportsKeywords []string
	Clock          func() time.Time
}

// Loop owns the analytics state.
type Loop struct {
	cfg      Config
	engine   *aggregate.Engine
	registry *aggregate.MarketRegistry
	baseline *baseline.Baseline
	cluster  *cluster.Detector
	emit     Emitter
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time
}

// New wires the analytics state. Baseline.Window doubles as the lookback for
// per-(category, market, outcome) reservoirs.
func New(
	cfg Config,
	eng *aggregate.Engine,
	reg *aggregate.MarketRegistry,
	emit Emitter,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
	if cfg.GaugeInterval <= 0 {
		cfg.GaugeInterval = time.Minute
	}
	if cfg.GrafanaContext <= 0 {
		cfg.GrafanaContext = time.Hour
	}
	if cfg.Filter == nil {
		cfg.Filter = category.NewFilter(nil)
	}
	if cfg.LifecycleAlertFromPct < 0 {
		cfg.LifecycleAlertFromPct = 0
	}
	if cfg.LifecycleHotFromPct < cfg.LifecycleAlertFromPct {
		cfg.LifecycleHotFromPct = cfg.LifecycleAlertFromPct
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	// Propagate the clock into sub-detectors so tests stay deterministic.
	if cfg.Baseline.Clock == nil {
		cfg.Baseline.Clock = now
	}
	if cfg.Cluster.Clock == nil {
		cfg.Cluster.Clock = now
	}
	return &Loop{
		cfg:      cfg,
		engine:   eng,
		registry: reg,
		baseline: baseline.New(cfg.Baseline),
		cluster:  cluster.New(cfg.Cluster),
		emit:     emit,
		metrics:  m,
		log:      log,
		now:      now,
	}
}

// Observe is the per-trade hot path called by collect for every ingested trade.
// Safe for concurrent calls.
//
// Steps (per category the market belongs to):
//  1. Read current baseline stats for the bucket.
//  2. Add the trade to the bucket (post-read so the new sample doesn't
//     pollute its own baseline).
//  3. Score; on fire, push into cluster + remember best-severity context for
//     the single-trade alert.
//  4. After looping all categories, emit at most one single-trade Finding
//     (with the highest-severity category as context).
func (l *Loop) Observe(ctx context.Context, market market.Market, trade trade.Trade) {
	if trade.Size <= 0 || trade.Price <= 0 {
		return
	}
	notional := trade.NotionalUSD()
	if notional <= 0 {
		return
	}
	l.metrics.TradeSizeUSD.Observe(notional)

	// Alert-eligibility gates. These do NOT block baseline updates — we want
	// the reservoir to warm continuously so it's ready the moment the market
	// crosses the lifecycle threshold.
	lifecyclePct, lifecycleKnown := market.LifecyclePct(trade.Timestamp)
	hot := lifecycleKnown && lifecyclePct >= l.cfg.LifecycleHotFromPct
	gateAllowsAlert := true
	if !lifecycleKnown && !l.cfg.AllowUnknownMarketLifecycle {
		// Fail-closed: a market without start/end dates can't be lifecycle-gated
		// and so can't be trusted by default.
		gateAllowsAlert = false
	}
	if lifecycleKnown && lifecyclePct < l.cfg.LifecycleAlertFromPct {
		gateAllowsAlert = false
	}
	if l.cfg.MarketMinAge > 0 && !market.StartDate.IsZero() &&
		l.now().Sub(market.StartDate) < l.cfg.MarketMinAge {
		gateAllowsAlert = false
	}
	// Secondary sports check on the market itself (title, slug, event slug)
	// — catches sports markets attached to non-sports categories like
	// "Hide From New".
	if gateAllowsAlert && marketLooksLikeSport(market, l.cfg.SportsKeywords) {
		gateAllowsAlert = false
	}

	categories := market.Categories
	if len(categories) == 0 {
		// Bucket trades from un-categorised markets under category id 0 so the
		// signal is still seen — it just won't roll up to a named category.
		categories = []vo.CategoryID{0}
	}

	var (
		bestCat    vo.CategoryID
		bestStats  baseline.Stats
		bestResult score.Result
		bestRef    anomaly.TradeRef
	)
	for _, cat := range categories {
		// Defense in depth: discover should have stripped blacklisted ids
		// before they reached the registry, but a missed entry must not be
		// able to fire an alert here either.
		if !l.allowed(cat) {
			l.metrics.CategoryFilterSkipped.WithLabelValues("detect").Inc()
			continue
		}
		bucket := baseline.Key{Category: cat, Market: market.ID, OutcomeToken: trade.Token}
		stats := l.baseline.Stats(bucket)
		l.baseline.Add(bucket, notional, trade.Timestamp)

		// Alert-eligibility gates from here down (baseline already updated).
		if !gateAllowsAlert {
			continue
		}
		// Baseline readiness: insist on a minimum observed span. This is
		// distinct from BaselineWindow (the maximum cap) — a 1-month market
		// is fine on a 1y cap, but we still want at least, say, 24h of
		// observed activity before we trust the median.
		if l.cfg.BaselineMinReadySpan > 0 && stats.SpanActual < l.cfg.BaselineMinReadySpan {
			continue
		}

		sr := score.Score(notional, trade.Price, stats, l.cfg.Thresholds)
		if !sr.Fired {
			continue
		}

		ref := l.buildTradeRef(market, trade, notional)
		if cs := l.cluster.Observe(cat, ref); cs != nil {
			l.emitCategoryWatch(ctx, market, trade, cat, cs)
		}

		// Keep the highest-severity category as the single-trade alert context.
		if !bestResult.Fired || anomaly.MaxSeverity(sr.Severity, bestResult.Severity) == sr.Severity {
			bestCat = cat
			bestStats = stats
			bestResult = sr
			bestRef = ref
		}
	}
	if bestResult.Fired {
		l.emitTradeAnomaly(ctx, market, trade, bestCat, bestStats, bestResult, bestRef, lifecyclePct, hot)
	}
}

func (l *Loop) buildTradeRef(m market.Market, t trade.Trade, notional float64) anomaly.TradeRef {
	var odds float64
	if t.Price > 0 {
		odds = 1.0 / t.Price
	}
	return anomaly.TradeRef{
		ID:          t.ID,
		TxHash:      t.TxHash,
		Wallet:      t.Taker,
		Market:      m.ID,
		Slug:        m.Slug,
		Question:    m.Question,
		Outcome:     m.OutcomeLabel(t.Token),
		Side:        t.Side,
		SizeShares:  t.Size,
		Price:       t.Price,
		Odds:        odds,
		NotionalUSD: notional,
		At:          t.Timestamp,
	}
}

func (l *Loop) emitTradeAnomaly(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	cat vo.CategoryID,
	stats baseline.Stats,
	sr score.Result,
	ref anomaly.TradeRef,
	lifecyclePct float64,
	hot bool,
) {
	catRef := l.categoryRef(cat)
	scope := fmt.Sprintf("category=%s market=%s outcome=%s",
		nonEmpty(catRef.Label, "uncategorised"), m.Slug, nonEmpty(ref.Outcome, "?"))
	peerCount := l.cluster.Count(cat)
	f := anomaly.Finding{
		Kind:     anomaly.KindTradeAnomaly,
		Severity: sr.Severity,
		At:       l.now(),
		Reason:   anomaly.ReasonSingle,
		Trade:    &ref,
		Category: &catRef,
		Baseline: &anomaly.BaselineRef{
			Scope:     scope,
			MedianUSD: stats.MedianUSD,
			MeanUSD:   stats.MeanUSD,
			P95USD:    stats.P95USD,
			SampleN:   stats.Count,
			Span:      stats.SpanActual,
			WindowMax: l.cfg.Baseline.Window,
		},
		Multiplier:       sr.Multiplier,
		AbsoluteTier:     sr.AbsoluteTier,
		MultiplierTier:   sr.MultiplierTier,
		LifecyclePct:     lifecyclePct,
		Hot:              hot,
		InCluster:        peerCount >= 2,
		ClusterPeerCount: peerCount,
		MarketURL:        l.marketURL(m),
		CategoryURL:      l.categoryURL(catRef),
		TraderURL:        l.traderURL(ref.Wallet),
		GrafanaURL:       l.grafanaURL(catRef, m, t.Timestamp, sr.Severity),
	}
	l.metrics.TradeAnomalies.WithLabelValues(string(sr.Severity), categoryLabel(catRef), anomaly.ReasonSingle).Inc()
	if sr.Multiplier > 0 {
		l.metrics.TradeAnomalyMultiplier.Observe(sr.Multiplier)
	}
	if ref.Odds > 0 {
		l.metrics.TradeOdds.Observe(ref.Odds)
	}
	l.metrics.CategoryAnomalousTrades.WithLabelValues(categoryLabel(catRef), string(sr.Severity)).Inc()
	l.metrics.CategoryAnomalousUSD.WithLabelValues(categoryLabel(catRef), string(sr.Severity)).Add(ref.NotionalUSD)
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit single-trade failed")
	}
}

func (l *Loop) emitCategoryWatch(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	cat vo.CategoryID,
	cs *anomaly.ClusterStats,
) {
	catRef := l.categoryRef(cat)
	f := anomaly.Finding{
		Kind:        anomaly.KindCategoryWatch,
		Severity:    anomaly.SeverityHard,
		At:          l.now(),
		Reason:      anomaly.ReasonCluster,
		Category:    &catRef,
		Cluster:     cs,
		MarketURL:   l.marketURL(m),
		CategoryURL: l.categoryURL(catRef),
		GrafanaURL:  l.grafanaURL(catRef, market.Market{}, t.Timestamp, anomaly.SeverityHard),
	}
	l.metrics.CategoryHardAlerts.WithLabelValues(categoryLabel(catRef)).Inc()
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit category-watch failed")
	}
}

// marketLooksLikeSport scans the market's title/slug/event-slug for any
// blacklisted sports keyword (case-insensitive substring). Empty keyword list
// disables the check.
func marketLooksLikeSport(m market.Market, keywords []string) bool {
	if len(keywords) == 0 {
		return false
	}
	hay := strings.ToLower(m.Question + " " + m.Slug + " " + m.EventSlug + " " + m.EventTitle)
	for _, k := range keywords {
		if k == "" {
			continue
		}
		if strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// allowed reports whether the category passes the blacklist. Uncategorised
// (id=0) always passes — we still want to score uncategorised whales.
func (l *Loop) allowed(cat vo.CategoryID) bool {
	if cat == 0 {
		return true
	}
	if c, ok := l.registry.Category(cat); ok {
		return l.cfg.Filter.Allowed(c.Slug, c.Label)
	}
	return true
}

func (l *Loop) categoryRef(cat vo.CategoryID) anomaly.CategoryRef {
	ref := anomaly.CategoryRef{ID: cat}
	if c, ok := l.registry.Category(cat); ok {
		ref.Slug = c.Slug
		ref.Label = c.Label
	}
	return ref
}

// marketURL returns the user-facing Polymarket event page URL for m. We
// deliberately key on the EVENT slug, not the market slug — sub-card markets
// (e.g. one team's leg of the World Cup winner event) 404 when addressed by
// market slug. When the event slug is missing we return "" rather than emit a
// known-broken /event/<market-slug> URL.
func (l *Loop) marketURL(m market.Market) string {
	return l.polymarketPath("event", m.EventSlug)
}

// categoryURL produces a /predictions/<slug> link. Verified live: Polymarket
// 308-redirects /markets/<slug> to /predictions/<slug>; we emit the canonical
// destination directly so the click doesn't pay a redirect round-trip.
func (l *Loop) categoryURL(c anomaly.CategoryRef) string {
	return l.polymarketPath("predictions", c.Slug)
}

// traderURL produces a /profile/<wallet> link.
func (l *Loop) traderURL(wallet string) string {
	return l.polymarketPath("profile", wallet)
}

func (l *Loop) polymarketPath(segs ...string) string {
	if l.cfg.PolymarketBase == "" {
		return ""
	}
	for _, s := range segs {
		if s == "" {
			return ""
		}
	}
	u, err := url.Parse(l.cfg.PolymarketBase)
	if err != nil {
		return ""
	}
	u.Path = singleSlashJoin(u.Path, segs...)
	return u.String()
}

// grafanaURL builds a deep-link with from/to ±GrafanaContext around `at` and
// the right dashboard variables. Empty when not configured.
func (l *Loop) grafanaURL(cat anomaly.CategoryRef, m market.Market, at time.Time, sev anomaly.Severity) string {
	if l.cfg.GrafanaBase == "" || l.cfg.GrafanaDashUID == "" {
		return ""
	}
	u, err := url.Parse(l.cfg.GrafanaBase)
	if err != nil {
		return ""
	}
	u.Path = singleSlashJoin(u.Path, "d", l.cfg.GrafanaDashUID) + "/"

	q := url.Values{}
	q.Set("orgId", "1")
	q.Set("from", strconv.FormatInt(at.Add(-l.cfg.GrafanaContext).UnixMilli(), 10))
	q.Set("to", strconv.FormatInt(at.Add(l.cfg.GrafanaContext).UnixMilli(), 10))
	if cat.Label != "" {
		q.Set("var-category", cat.Label)
	}
	if m.Slug != "" {
		q.Set("var-market", m.Slug)
	}
	if sev != "" {
		q.Set("var-severity", string(sev))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// singleSlashJoin joins path segments with exactly one "/" between them,
// preserving a leading slash on the base path.
func singleSlashJoin(base string, segs ...string) string {
	out := base
	for _, s := range segs {
		if s == "" {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != '/' {
			out += "/"
		}
		out += s
	}
	return out
}

// Run periodically refreshes supporting Grafana gauges from the aggregate
// engine. It does NOT fire any alerts — alerts are emitted synchronously from
// Observe. Run blocks until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) error {
	t := time.NewTicker(l.cfg.GaugeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.refreshGauges()
		}
	}
}

func (l *Loop) refreshGauges() {
	for _, m := range l.registry.Snapshot() {
		for _, w := range l.cfg.RecentWindows {
			win := l.engine.Window(m.ID, w)
			label := windowLabel(w)
			l.metrics.WindowTradeRate.WithLabelValues(string(m.ID), label).Set(win.TradesPerMinute())
			l.metrics.WindowNotionalRate.WithLabelValues(string(m.ID), label).Set(win.NotionalPerMinute())
			l.metrics.WindowAvgSize.WithLabelValues(string(m.ID), label).Set(win.AvgSize())
		}
	}
	l.metrics.BaselineBuckets.Set(float64(l.baseline.Buckets()))
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

func categoryLabel(c anomaly.CategoryRef) string {
	if c.Label != "" {
		return c.Label
	}
	return "uncategorized"
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
