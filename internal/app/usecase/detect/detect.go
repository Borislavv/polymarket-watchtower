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
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
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
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/rs/zerolog"
)

// Emitter receives findings for realtime telemetry sinks (log, webhook).
// Telegram delivery in production does NOT flow through this emitter — it
// is dispatched by the alertsender worker reading from polymarket_alerts.
type Emitter interface {
	Notify(ctx context.Context, f anomaly.Finding) error
}

// BaselineFetcher is the read-only baseline statistics interface consumed
// by the per-trade scorer. In production it is satisfied by
// internal/app/usecase/analytics/dbbaseline.Provider; tests rely on the
// embedded in-memory analytics/baseline.Baseline (no fetcher configured).
type BaselineFetcher interface {
	Stats(ctx context.Context, k baseline.Key) (baseline.Stats, error)
}

// AlertCreator is the dedup primitive. TryCreatePending must:
//   - return created=true with a fresh Alert when the dedup_key is new;
//   - return created=false (no error) when the dedup_key already exists.
//
// Satisfied by *repository.AlertRepository. When nil, the detector emits
// realtime to the configured Emitter without DB dedup — a memory/debug
// shape used only by tests.
type AlertCreator interface {
	TryCreatePending(ctx context.Context, a repository.NewAlert) (repository.Alert, bool, error)
}

// MarketResolver maps a Polymarket condition id to the local market row,
// used to populate alerts.market_id and to namespace cluster dedup keys.
type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

// TraderResolver maps a wallet to the local trader row, used to populate
// alerts.trader_id. Returns repository.ErrTraderNotFound when unseen — the
// detector treats that as "no trader fk" rather than an error.
type TraderResolver interface {
	GetByWallet(ctx context.Context, wallet string) (repository.Trader, error)
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
	Clock                       func() time.Time

	// Baseliner overrides the in-memory baseline reservoir. When set, the
	// detector queries it on every Observe and does NOT seed the in-process
	// ring (trade ingestion to the DB is owned by persist.Sink + backfill
	// worker — that's what the fetcher reads from). Leave nil to use the
	// embedded baseline.Baseline (local/debug, BASELINE_SOURCE=memory).
	Baseliner BaselineFetcher
	// Alerts wires the Postgres dedup primitive. When set, every fired
	// Finding is INSERT … ON CONFLICT DO NOTHING into polymarket_alerts
	// before being handed to the realtime Emitter. Conflicts suppress the
	// emit entirely so log/webhook stay in sync with the DB queue.
	Alerts AlertCreator
	// Markets resolves condition_id → DB market id for the alerts row.
	Markets MarketResolver
	// Traders resolves wallet → DB trader id for the alerts row.
	Traders TraderResolver
	// StrategyVersion is stamped on every alert row and woven into the
	// dedup_key so a config retune cannot ressurect ignored alerts.
	// Defaults to "v1".
	StrategyVersion string
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
	if cfg.StrategyVersion == "" {
		cfg.StrategyVersion = "v1"
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

// Observe is the per-trade hot path called by collect for every ingested
// trade. Safe for concurrent calls.
//
// Production wiring (cfg.Baseliner + cfg.Alerts set):
//  1. Read baseline stats from Postgres (the trade was already persisted by
//     persist.Sink before Observe ran, so the DB reflects the latest state).
//  2. Score against thresholds; on fire, attempt to insert the alert row
//     into polymarket_alerts with a dedup_key derived from the trade.
//  3. On a fresh insert, also notify realtime sinks (log/webhook) and feed
//     the in-process cluster window for HARD detection. The Telegram sink
//     is NOT in this fanout — the alertsender worker reads pending rows.
//
// Memory wiring (no Baseliner, no Alerts — tests and local debug):
//   - Baseline stats come from the embedded in-memory reservoir; the trade
//     is added to that reservoir for future scoring rounds.
//   - Findings go directly to the realtime emitter; no DB dedup.
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
		// Defense in depth: discover should have stripped non-whitelisted
		// category ids before they reached the registry, but a leak must
		// not be able to fire an alert here either.
		if !l.allowed(cat) {
			l.metrics.CategoryFilterSkipped.WithLabelValues("detect").Inc()
			continue
		}
		bucket := baseline.Key{Category: cat, Market: market.ID, OutcomeToken: trade.Token}
		stats, err := l.readBaseline(ctx, bucket, notional, trade.Timestamp)
		if err != nil {
			l.log.Err(err).Str("market", string(market.ID)).Msg("detect: baseline read failed")
			continue
		}

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

// readBaseline returns the per-bucket statistics. With cfg.Baseliner set
// (production), the DB is the source of truth and the in-memory ring is
// not touched — persist.Sink and the backfill worker are the writers.
// With no fetcher (tests/local), the in-memory reservoir is read and then
// updated with the current trade so the next call sees it.
func (l *Loop) readBaseline(ctx context.Context, k baseline.Key, notional float64, at time.Time) (baseline.Stats, error) {
	if l.cfg.Baseliner != nil {
		return l.cfg.Baseliner.Stats(ctx, k)
	}
	stats := l.baseline.Stats(k)
	l.baseline.Add(k, notional, at)
	return stats, nil
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

	dedup := l.singleTradeDedupKey(m, t)
	if !l.persistAlert(ctx, repository.AlertKindTrade, dedup, m, t, f) {
		// DB dedup said "already alerted" — keep realtime sinks in sync.
		return
	}
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

	dedup := l.clusterDedupKey(cat)
	if !l.persistAlert(ctx, repository.AlertKindCluster, dedup, m, t, f) {
		return
	}
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit category-watch failed")
	}
}

// persistAlert is the dedup gate. With cfg.Alerts wired, the alert row is
// inserted ON CONFLICT DO NOTHING; the bool reports whether this caller
// won the insert. With no AlertCreator (memory/debug), every call returns
// true so realtime emit proceeds — there is no DB dedup in that mode.
func (l *Loop) persistAlert(ctx context.Context, kind repository.AlertKind, dedupKey string, m market.Market, t trade.Trade, f anomaly.Finding) bool {
	if l.cfg.Alerts == nil {
		return true
	}
	payload, err := json.Marshal(f)
	if err != nil {
		l.log.Err(err).Msg("detect: marshal alert payload failed")
		return false
	}
	row := repository.NewAlert{
		DedupKey:        dedupKey,
		StrategyVersion: l.cfg.StrategyVersion,
		Kind:            kind,
		Reason:          f.Reason,
		Severity:        string(f.Severity),
		Payload:         payload,
		MarketID:        l.resolveMarketID(ctx, m.ID),
		TraderID:        l.resolveTraderID(ctx, t.Taker),
	}
	_, created, err := l.cfg.Alerts.TryCreatePending(ctx, row)
	if err != nil {
		l.log.Err(err).Str("dedup_key", dedupKey).Msg("detect: alert insert failed")
		return false
	}
	return created
}

// singleTradeDedupKey produces "single:<strategy>:<trade_dedup_key>". The
// trade_dedup_key matches the row written by repository.DedupKeyForTrade
// so an alert is exactly idempotent across restarts and concurrent
// observers.
func (l *Loop) singleTradeDedupKey(m market.Market, t trade.Trade) string {
	// The trade dedup_key derivation needs a market_id; for the alert key
	// the condition_id is equally stable and sidesteps a DB lookup. We
	// build a synthetic InsertTradeInput with the upstream ExternalID and
	// fall back to the composite hash when ExternalID is empty (rare).
	key := repository.DedupKeyForTrade(repository.InsertTradeInput{
		MarketID:     0, // not used when ExternalID is set; composite path
		OutcomeToken: string(t.Token) + "@" + string(m.ID),
		Side:         string(t.Side),
		Price:        t.Price,
		SizeShares:   t.Size,
		TradedAt:     t.Timestamp,
		ExternalID:   t.ID,
	})
	return "single:" + l.cfg.StrategyVersion + ":" + key
}

// clusterDedupKey produces "cluster:<strategy>:<category_id>:<window_start>".
// window_start floors `now` to the cluster cooldown so two cluster fires
// landing in the same cadence bucket dedup; the next bucket gets a fresh
// key, matching the cooldown contract.
func (l *Loop) clusterDedupKey(cat vo.CategoryID) string {
	bucket := l.cfg.Cluster.Cooldown
	if bucket <= 0 {
		bucket = l.cfg.Cluster.Window
	}
	if bucket <= 0 {
		bucket = 30 * time.Minute
	}
	windowStart := l.now().Truncate(bucket).Unix()
	return fmt.Sprintf("cluster:%s:%d:%d", l.cfg.StrategyVersion, int64(cat), windowStart)
}

// resolveMarketID returns a non-nil DB market id when cfg.Markets is wired
// and the row exists. Returns nil silently otherwise — the alerts.market_id
// column is nullable so callers can still file an alert against an as-yet-
// unpersisted market (discovery has not caught up).
func (l *Loop) resolveMarketID(ctx context.Context, condID vo.MarketID) *int64 {
	if l.cfg.Markets == nil || condID == "" {
		return nil
	}
	m, err := l.cfg.Markets.GetByConditionID(ctx, string(condID))
	if err != nil {
		return nil
	}
	id := m.ID
	return &id
}

// resolveTraderID returns a non-nil DB trader id when cfg.Traders is wired
// and the wallet has been seen. Returns nil for ErrTraderNotFound or any
// other lookup error.
func (l *Loop) resolveTraderID(ctx context.Context, wallet string) *int64 {
	if l.cfg.Traders == nil || wallet == "" {
		return nil
	}
	t, err := l.cfg.Traders.GetByWallet(ctx, wallet)
	if err != nil {
		if errors.Is(err, repository.ErrTraderNotFound) {
			return nil
		}
		return nil
	}
	id := t.ID
	return &id
}

// allowed reports whether the category passes the whitelist. Uncategorised
// (id=0) is treated as "not in any whitelist" and is blocked when the
// filter is active — we cannot affirmatively match an empty category to a
// whitelist token. With no whitelist configured the filter is disabled and
// everything passes.
func (l *Loop) allowed(cat vo.CategoryID) bool {
	if cat == 0 {
		return l.cfg.Filter.Allowed("", "")
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
