// Package metrics owns the Prometheus registry and the collectors used by the
// pipeline. Keeping the registry private prevents accidental use of the default
// global registry from random packages.
//
// Label-cardinality discipline:
//   - High-cardinality dimensions (market id, wallet, tx hash) live in LOGS
//     and ALERT PAYLOADS, never in counter labels — Polymarket has 5k+ active
//     markets and emitting them as labels would blow up Prometheus memory.
//   - Per-market counters (TradesIngested, NotionalIngested) are bounded by
//     the active universe and are cheap. The v4 cleanup removed the bucket-
//     only gauges (WindowTradeRate/NotionalRate/AvgSize) that fed off the
//     in-memory aggregate engine; replace with Postgres-derived Grafana
//     queries.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Registry is the type the /metrics HTTP handler consumes.
type Registry = *prometheus.Registry

// Metrics is the fixed set of collectors emitted by the app.
type Metrics struct {
	registry Registry

	// --- Upstream traffic ---
	UpstreamRequests *prometheus.CounterVec   // api, endpoint, status
	UpstreamLatency  *prometheus.HistogramVec // api, endpoint

	// --- Discovery ---
	MarketsTracked prometheus.Gauge

	// --- Collect (supporting per-market series — bounded by MAX_MARKETS) ---
	TradesIngested   *prometheus.CounterVec // market
	NotionalIngested *prometheus.CounterVec // market
	// --- Per-trade anomaly model (primary signal) ---
	TradeSizeUSD            prometheus.Histogram   // every trade's USD notional
	TradeOdds               prometheus.Histogram   // every trade's 1/price odds
	TradeAnomalyMultiplier  prometheus.Histogram   // observed effective multiplier for fired anomalies
	TraderMultiplier        prometheus.Histogram   // observed trader-axis multiplier when known
	TradeAnomalies          *prometheus.CounterVec // severity, category, reason
	TradeAnomalyAxis        *prometheus.CounterVec // axis = market|trader|both — which baseline drove the alert
	HighOddsTrades          *prometheus.CounterVec // severity, category — odds-driven anomalies
	CategoryAnomalousTrades *prometheus.CounterVec // category, severity
	CategoryAnomalousUSD    *prometheus.CounterVec // category, severity
	CategoryHardAlerts      *prometheus.CounterVec // category
	AccumulationAlerts      *prometheus.CounterVec // severity, category — same-trader accumulation lines
	QuietMarketAlerts       *prometheus.CounterVec // severity, kind — alerts stamped with QUIET_MARKET_WAKEUP context
	BaselineBuckets         prometheus.Gauge       // total live (category,market,outcome) buckets

	// --- Filtering ---
	CategoryFilterSkipped   *prometheus.CounterVec // stage = discover|detect
	AlertMMSuppressed       *prometheus.CounterVec // category, reason — alerts suppressed by MM/arb filter (reason=POSSIBLE_MARKET_MAKER)
	LifecycleUnknownSkipped prometheus.Counter     // trades silenced because the market had no StartDate/EndDate

	// --- Alerting outcomes ---
	TelegramAlertsSent  *prometheus.CounterVec // severity
	TelegramAlertErrors *prometheus.CounterVec // severity

	// --- Persistence (Postgres write path) ---
	// Counters increment exactly once per row inserted / updated. Operators
	// graph rate(...) over these to see whether the ingest path is keeping
	// up with the discover/collect/backfill cadence.
	MarketsUpserted         prometheus.Counter     // every successful UpsertMarket call (incl. ON CONFLICT)
	MarketOutcomesUpserted  prometheus.Counter     // every UpsertOutcome call (per token row)
	MarketsSoftDeleted      prometheus.Counter     // sweep-driven `active=false, deleted_at=NOW()` flips
	MarketsPurged           prometheus.Counter     // sanity reaper terminal state (`purged_at=NOW()`)
	MarketsResumed          prometheus.Counter     // sanity reaper detected a soft-deleted market back upstream
	TradesUpserted          prometheus.Counter     // unique trade rows inserted (excludes dedup_key conflicts)
	TradesDuplicatesSkipped prometheus.Counter     // UpsertBatch attempts that hit ON CONFLICT DO NOTHING
	TradersUpserted         prometheus.Counter     // distinct wallets persisted into polymarket_traders
	BackfillPagesFetched    prometheus.Counter     // Data API /trades pages successfully persisted
	BackfillRunsTotal       *prometheus.CounterVec // status = completed|partial_api_limit|failed

	// --- Stats summary worker ---
	StatsSummariesSent prometheus.Counter // periodic Telegram stats sends
	StatsSummaryErrors prometheus.Counter // periodic stats send failures
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	m.UpstreamRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "upstream", Name: "requests_total",
		Help: "HTTP requests issued to Polymarket upstreams, by api/endpoint/status.",
	}, []string{"api", "endpoint", "status"})

	m.UpstreamLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "upstream", Name: "request_duration_seconds",
		Help:    "Latency of Polymarket upstream HTTP calls.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
	}, []string{"api", "endpoint"})

	m.MarketsTracked = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "discover", Name: "markets_tracked",
		Help: "Number of markets currently in the active set.",
	})

	m.TradesIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "collect", Name: "trades_total",
		Help: "Trades ingested per market.",
	}, []string{"market"})

	m.NotionalIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "collect", Name: "notional_usd_total",
		Help: "Notional USD ingested per market.",
	}, []string{"market"})

	m.TradeSizeUSD = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "size_usd",
		Help: "USD notional of every ingested trade.",
		// $10 -> $1M, 12 buckets — covers retail through whale.
		Buckets: []float64{10, 50, 100, 500, 1_000, 3_000, 10_000, 30_000, 100_000, 300_000, 1_000_000, 10_000_000},
	})

	m.TradeOdds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "odds",
		Help:    "Implied odds (1/price) of every ingested trade.",
		Buckets: []float64{1, 1.5, 2, 3, 5, 10, 25, 50, 100, 1000},
	})

	m.TradeAnomalyMultiplier = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "anomaly_multiplier",
		Help:    "Observed effective notional/baseline multiplier when a single-trade anomaly fires (max of market and trader axes).",
		Buckets: []float64{10, 30, 100, 300, 1_000, 3_000, 10_000, 100_000, 1_000_000},
	})

	m.TraderMultiplier = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "trader_multiplier",
		Help:    "Observed trader-axis multiplier (notional / trader history median) when the trader baseline was available.",
		Buckets: []float64{1, 3, 10, 30, 100, 300, 1_000, 3_000, 10_000, 100_000},
	})

	m.TradeAnomalies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "anomalies_total",
		Help: "Single-trade anomalies emitted, by severity/category/reason.",
	}, []string{"severity", "category", "reason"})

	m.TradeAnomalyAxis = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "anomaly_axis_total",
		Help: "Single-trade anomalies by the multiplier axis that drove the tier (market|trader|both).",
	}, []string{"axis"})

	m.HighOddsTrades = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "high_odds_total",
		Help: "Single-trade anomalies whose reason is HighOddsTrade or HighOddsWhaleDetected.",
	}, []string{"severity", "category"})

	m.CategoryAnomalousTrades = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "category", Name: "anomalous_trades_total",
		Help: "Anomalous trades attributed to a category, by severity.",
	}, []string{"category", "severity"})

	m.CategoryAnomalousUSD = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "category", Name: "anomalous_notional_usd_total",
		Help: "Anomalous USD notional attributed to a category, by severity.",
	}, []string{"category", "severity"})

	m.CategoryHardAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "category", Name: "hard_alerts_total",
		Help: "CategoryWatchRequired (HARD) alerts emitted, by category.",
	}, []string{"category"})

	m.AccumulationAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "accumulation", Name: "alerts_total",
		Help: "Same-trader accumulation-line alerts emitted, by severity and category.",
	}, []string{"severity", "category"})

	m.QuietMarketAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "quietmarket", Name: "alerts_total",
		Help: "Alerts stamped with QUIET_MARKET_WAKEUP context, by severity and finding kind.",
	}, []string{"severity", "kind"})

	m.BaselineBuckets = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "baseline", Name: "buckets",
		Help: "Number of live (category, market, outcome) baseline buckets.",
	})

	m.CategoryFilterSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "filter", Name: "category_skipped_total",
		Help: "Times a category was skipped by the whitelist, by stage (discover|detect).",
	}, []string{"stage"})

	m.AlertMMSuppressed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "filter", Name: "alert_mm_suppressed_total",
		Help: "Alerts suppressed because the wallet showed balanced two-sided activity (market-making/arbitrage signature). Labelled by category and the structured reason code (POSSIBLE_MARKET_MAKER).",
	}, []string{"category", "reason"})

	m.LifecycleUnknownSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "filter", Name: "lifecycle_unknown_skipped_total",
		Help: "Trades silenced because the market lacked StartDate/EndDate (lifecycle gate is fail-closed without exception in v4).",
	})

	m.TelegramAlertsSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "telegram", Name: "alerts_sent_total",
		Help: "Telegram alerts successfully delivered, by severity.",
	}, []string{"severity"})

	m.TelegramAlertErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "telegram", Name: "alert_errors_total",
		Help: "Telegram alert delivery failures, by severity.",
	}, []string{"severity"})

	m.MarketsUpserted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "persist", Name: "markets_upserted_total",
		Help: "Successful UpsertMarket calls. Includes both fresh inserts and updates to existing rows (ON CONFLICT path).",
	})
	m.MarketOutcomesUpserted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "persist", Name: "market_outcomes_upserted_total",
		Help: "Successful UpsertOutcome calls — one per (market, token) row written.",
	})
	m.MarketsSoftDeleted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "persist", Name: "markets_soft_deleted_total",
		Help: "Markets flipped to active=false with deleted_at=NOW() by a discovery sweep (disappeared upstream).",
	})
	m.MarketsPurged = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "sanity", Name: "markets_purged_total",
		Help: "Soft-deleted markets that reached retention and were marked purged_at by the sanity reaper. Trade rows are retained.",
	})
	m.MarketsResumed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "sanity", Name: "markets_resumed_total",
		Help: "Soft-deleted markets that reappeared upstream during the retention window and were requeued for backfill.",
	})
	m.TradesUpserted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "persist", Name: "trades_upserted_total",
		Help: "Unique trade rows persisted. Excludes attempts whose dedup_key collided with an existing row.",
	})
	m.TradesDuplicatesSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "persist", Name: "trades_duplicates_skipped_total",
		Help: "Trade insert attempts dropped by the dedup_key UNIQUE constraint (ON CONFLICT DO NOTHING). Overlapping collect/backfill sweeps inflate this counter; high values are not an error.",
	})
	m.TradersUpserted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "persist", Name: "traders_upserted_total",
		Help: "Wallets persisted into polymarket_traders. Counts every UpsertSeen row, so per-tick churn is expected as the same wallets reappear.",
	})
	m.BackfillPagesFetched = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "backfill", Name: "pages_fetched_total",
		Help: "Data API /trades pages successfully persisted by the backfill worker. Multiply by configured PageSize for an upper bound on rows touched.",
	})
	m.BackfillRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "backfill", Name: "runs_total",
		Help: "Backfill runs that reached a terminal state, labelled by outcome (completed, partial_api_limit, failed).",
	}, []string{"status"})

	m.StatsSummariesSent = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "stats", Name: "summaries_sent_total",
		Help: "Periodic Telegram stats summaries delivered (one per interval when the worker is enabled).",
	})
	m.StatsSummaryErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "stats", Name: "summary_errors_total",
		Help: "Periodic stats summary send failures. Non-zero usually means Telegram delivery or a stats query is broken.",
	})

	reg.MustRegister(
		m.UpstreamRequests, m.UpstreamLatency,
		m.MarketsTracked,
		m.TradesIngested, m.NotionalIngested,
		m.TradeSizeUSD, m.TradeOdds, m.TradeAnomalyMultiplier, m.TraderMultiplier,
		m.TradeAnomalies, m.TradeAnomalyAxis, m.HighOddsTrades,
		m.CategoryAnomalousTrades, m.CategoryAnomalousUSD, m.CategoryHardAlerts,
		m.AccumulationAlerts,
		m.QuietMarketAlerts,
		m.BaselineBuckets,
		m.CategoryFilterSkipped, m.AlertMMSuppressed, m.LifecycleUnknownSkipped,
		m.TelegramAlertsSent, m.TelegramAlertErrors,
		m.MarketsUpserted, m.MarketOutcomesUpserted,
		m.MarketsSoftDeleted, m.MarketsPurged, m.MarketsResumed,
		m.TradesUpserted, m.TradesDuplicatesSkipped, m.TradersUpserted,
		m.BackfillPagesFetched, m.BackfillRunsTotal,
		m.StatsSummariesSent, m.StatsSummaryErrors,
	)
	return m
}

// Registry exposes the underlying registry for the HTTP handler.
func (m *Metrics) Registry() Registry { return m.registry }

// UpstreamObserver returns a callback that increments UpstreamRequests +
// observes UpstreamLatency for the given API label.
func (m *Metrics) UpstreamObserver(api string) func(endpoint string, status int, dur time.Duration) {
	return func(endpoint string, status int, dur time.Duration) {
		m.UpstreamRequests.WithLabelValues(api, endpoint, statusLabel(status)).Inc()
		m.UpstreamLatency.WithLabelValues(api, endpoint).Observe(dur.Seconds())
	}
}

func statusLabel(status int) string {
	switch {
	case status == 0:
		return "net_err"
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status == 429:
		return "429"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
