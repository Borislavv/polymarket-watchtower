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
	TradeMarketP95Ratio     prometheus.Histogram   // notional / market.p95 for fired anomalies
	TradeTraderP95Ratio     prometheus.Histogram   // notional / trader.p95 for fired anomalies (when trader axis enforced)
	TradeProfitIfWinUSD     prometheus.Histogram   // profit if win = notional × (odds − 1) for fired anomalies
	TradeAnomalies          *prometheus.CounterVec // severity, category, reason
	HighOddsTrades          *prometheus.CounterVec // severity, category — odds-driven anomalies
	CategoryAnomalousTrades *prometheus.CounterVec // category, severity
	CategoryAnomalousUSD    *prometheus.CounterVec // category, severity
	CategoryHardAlerts      *prometheus.CounterVec // category
	AccumulationAlerts      *prometheus.CounterVec // severity, category, window={recent|lifetime}
	OwnershipAlerts         *prometheus.CounterVec // severity, category — Strategy-E ownership_concentration fires
	NewWalletReasons        *prometheus.CounterVec // kind, severity — context boosters attached
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
	// TradesImported counts the raw INGESTION rate per source
	// (collect | backfill). Distinct from TradesUpserted which only
	// counts FRESH inserts — TradesImported includes duplicates so
	// the operator can see "collect is still pulling, backfill is
	// double-counting" patterns. The diagnostic that motivates this:
	// for 24h of data, ingested_at counted 897k rows while traded_at
	// counted 109k — 8× firehose from backfill.
	TradesImported *prometheus.CounterVec // source = collect|backfill

	// TradesAnalyzed counts the trades that reached detect.Observe.
	// In a healthy pipeline this should track TradesImported{source=collect}
	// modulo a small skip pile (too_old_for_live_alert, etc.).
	// Divergence between imported and analyzed is the structural
	// signal that the collect cursor is poisoned.
	TradesAnalyzed prometheus.Counter

	// TradesAnalyzedTotal is the v6 detection-queue counter: per
	// trade, the worker stamps one of {analyzed | skipped | failed}
	// with an optional reason. status = {analyzed,skipped,failed}.
	// reason is the skip/failure cause (empty for status=analyzed).
	TradesAnalyzedTotal *prometheus.CounterVec // status, reason

	// TradesSkippedDetection records every trade detect.Observe
	// declined to score. The reason label is the typed string from
	// detect.SkipReason* (currently: too_old_for_live_alert).
	TradesSkippedDetection *prometheus.CounterVec // reason

	// DetectionClaimed counts trades the detection worker pulled out
	// of the queue. Useful for sanity-checking the worker is actually
	// running.
	DetectionClaimed prometheus.Counter

	// DetectionFailed counts terminal failures during detection
	// (claim errors, mark errors, panics). reason: claim_error |
	// panic | mark_analyzed.
	DetectionFailed *prometheus.CounterVec // reason

	// DetectionLagSeconds is the histogram of (now − traded_at) at
	// the moment the worker dequeues a trade. The right tail tells
	// operators how stale the backlog gets.
	DetectionLagSeconds prometheus.Histogram

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

	// --- Signal-quality reports + reactions (Strategy reporting) ---
	// SignalReportsSent: labelled by period_type (daily / weekly /
	// monthly / quarterly / yearly) and status (sent / failed).
	// TelegramReactions: labelled by status (applied / unsupported /
	// failed / disabled) and reaction (the emoji applied, or "" when
	// the call never reached Telegram).
	// AlertOutcomes: labelled by status (resolved_correct /
	// resolved_wrong / unknown / unavailable), severity, kind — used
	// by Grafana to show signal quality over time without re-running
	// the aggregate SQL.
	SignalReportsSent *prometheus.CounterVec // period_type, status
	TelegramReactions *prometheus.CounterVec // status, reaction
	AlertOutcomes     *prometheus.CounterVec // status, severity, kind

	// --- PAL · Proof of Alert Value ---
	// AlertRealizedEdge is a HistogramVec — the only Prometheus type
	// whose _sum field admits negative observations. Buckets are
	// chosen so the [-1, +1] range any single edge can take is
	// covered with informative resolution at the centre.
	AlertRealizedEdge *prometheus.HistogramVec // severity, kind

	// AlertWeightedSuccessTotal accumulates severity_weight ×
	// success_binary per resolved alert. Denominator is
	// AlertWeightedResolvedTotal (severity_weight per resolved
	// alert). Their ratio is the weighted success rate; the
	// Grafana panel computes it as
	//   sum(rate(success)) / sum(rate(resolved))
	AlertWeightedSuccessTotal  *prometheus.CounterVec // severity, kind
	AlertWeightedResolvedTotal *prometheus.CounterVec // severity, kind

	// AlertCalibrationTotal counts every classified alert by its
	// implied-probability bucket. The 4 labels add up to bounded
	// cardinality: 7 buckets × 4 statuses × 4 severities × ~5 kinds
	// = 560 series cap. Cheap.
	AlertCalibrationTotal *prometheus.CounterVec // bucket, status, severity, kind
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

	m.TradeMarketP95Ratio = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "market_p95_ratio",
		Help:    "Observed notional / market-p95 ratio when a single-trade anomaly fires. 0 when the market baseline was not ready.",
		Buckets: []float64{0.5, 1, 2, 3, 5, 10, 30, 100, 300, 1_000},
	})

	m.TradeTraderP95Ratio = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "trader_p95_ratio",
		Help:    "Observed notional / trader-p95 ratio when a single-trade anomaly fires. 0 when the trader baseline was not ready.",
		Buckets: []float64{0.5, 1, 1.5, 2, 3, 5, 10, 30, 100},
	})

	m.TradeProfitIfWinUSD = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "profit_if_win_usd",
		Help:    "Profit if the firing trade resolves favourably (notional × (odds-1)).",
		Buckets: []float64{1_000, 5_000, 15_000, 50_000, 100_000, 250_000, 1_000_000, 10_000_000},
	})

	m.TradeAnomalies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trade", Name: "anomalies_total",
		Help: "Single-trade anomalies emitted, by severity/category/reason.",
	}, []string{"severity", "category", "reason"})

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
		Help: "Same-trader accumulation-line alerts emitted, by severity, category, and window (recent|lifetime).",
	}, []string{"severity", "category", "window"})

	m.OwnershipAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ownership", Name: "alerts_total",
		Help: "Market-ownership concentration alerts emitted, by severity and category. Trade-flow approximation — see strategy doc.",
	}, []string{"severity", "category"})

	m.NewWalletReasons = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "newwallet", Name: "reasons_attached_total",
		Help: "New-wallet context booster: count of Findings that picked up a NEW_WALLET_* reason, by parent alert kind and severity.",
	}, []string{"kind", "severity"})

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

	m.TradesImported = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trades", Name: "imported_total",
		Help: "Trades persisted into polymarket_trades, by source (collect|backfill). Counts EVERY attempt including duplicates — divergence from trades_analyzed_total tells you the collect cursor isn't keeping up with the live tail.",
	}, []string{"source"})
	m.TradesAnalyzed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trades", Name: "analyzed_total",
		Help: "Trades that reached detect.Observe and were scored against the strategy gates. A growing gap between this counter and trades_imported_total{source=collect} is the canonical signal that backfill is consuming the live tail.",
	})
	m.TradesSkippedDetection = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trades", Name: "skipped_detection_total",
		Help: "Trades that reached detect.Observe but were not scored, by reason. Currently the only reason emitted is `too_old_for_live_alert` (LIVE_ALERT_MAX_LAG); the metric exists with a label vector so future skip paths are loud.",
	}, []string{"reason"})
	m.TradesAnalyzedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "trades", Name: "analyzed_status_total",
		Help: "v6 detection-queue terminal state per trade. status=analyzed|skipped|failed; reason carries the skip/failure cause (empty when status=analyzed).",
	}, []string{"status", "reason"})
	m.DetectionClaimed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "detection", Name: "claimed_total",
		Help: "Trades the detection worker pulled out of the pending queue. Health check — should track imports modulo lag.",
	})
	m.DetectionFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "detection", Name: "failed_total",
		Help: "Detection failures. reason: claim_error | panic | mark_analyzed.",
	}, []string{"reason"})
	m.DetectionLagSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "detection", Name: "lag_seconds",
		Help:    "now() − traded_at when the worker dequeues a trade. Right-tail tells operators how stale the backlog is getting.",
		Buckets: []float64{1, 5, 15, 60, 300, 900, 3600, 7200, 21600, 86400},
	})

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

	m.SignalReportsSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "signal", Name: "reports_sent_total",
		Help: "Scheduled signal-quality reports delivered to Telegram, by period_type (daily / weekly / monthly / quarterly / yearly) and status (sent / failed).",
	}, []string{"period_type", "status"})

	m.TelegramReactions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "telegram", Name: "reactions_total",
		Help: "Outcome reactions applied to original alert messages, by status (applied / unsupported / failed / disabled) and reaction (the emoji used; empty when the call did not reach Telegram).",
	}, []string{"status", "reaction"})

	m.AlertOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "alert", Name: "outcomes_total",
		Help: "Resolved alert verdicts, by status (resolved_correct / resolved_wrong / unknown / unavailable), severity, and alert kind. Drives the Grafana signal-quality panels.",
	}, []string{"status", "severity", "kind"})

	// PAL · Proof of Alert Value
	// HistogramVec admits negative observations on _sum (unlike Counter).
	// Buckets span the legal range [-1, +1] of realized edge.
	m.AlertRealizedEdge = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "alert", Name: "realized_edge",
		Help:    "PAL · realized edge per resolved alert: success_binary − implied_probability_at_alert. Sum over resolved alerts (PromQL: rate(sum) / rate(count)) is the average edge. Positive average means alerts beat the market's implied probability — the load-bearing proof-of-value metric.",
		Buckets: []float64{-1.0, -0.75, -0.5, -0.25, -0.10, 0, 0.10, 0.25, 0.50, 0.75, 1.0},
	}, []string{"severity", "kind"})
	m.AlertWeightedSuccessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "alert", Name: "weighted_success_total",
		Help: "PAL · severity-weighted successes: sum of severity_weight × 1{resolved_correct}. Weights: Info=1 Warning=3 Critical=10 Hard=25. Divide by alert_weighted_resolved_total for the weighted success rate.",
	}, []string{"severity", "kind"})
	m.AlertWeightedResolvedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "alert", Name: "weighted_resolved_total",
		Help: "PAL · denominator for weighted success: sum of severity_weight per RESOLVED alert (pending/ambiguous/unavailable excluded).",
	}, []string{"severity", "kind"})
	m.AlertCalibrationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "alert", Name: "calibration_total",
		Help: "PAL · calibration: count of alerts by implied-probability bucket (0-10 / 10-20 / 20-30 / 30-40 / 40-50 / 50-70 / 70+), outcome status, severity, and kind. Low-bucket success rates above their implied probability is the signal-quality smoking gun.",
	}, []string{"bucket", "status", "severity", "kind"})

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
		m.TradeSizeUSD, m.TradeOdds,
		m.TradeMarketP95Ratio, m.TradeTraderP95Ratio, m.TradeProfitIfWinUSD,
		m.TradeAnomalies, m.HighOddsTrades,
		m.CategoryAnomalousTrades, m.CategoryAnomalousUSD, m.CategoryHardAlerts,
		m.AccumulationAlerts,
		m.OwnershipAlerts,
		m.NewWalletReasons,
		m.QuietMarketAlerts,
		m.BaselineBuckets,
		m.CategoryFilterSkipped, m.AlertMMSuppressed, m.LifecycleUnknownSkipped,
		m.TelegramAlertsSent, m.TelegramAlertErrors,
		m.TradesImported, m.TradesAnalyzed, m.TradesSkippedDetection,
		m.TradesAnalyzedTotal, m.DetectionClaimed, m.DetectionFailed, m.DetectionLagSeconds,
		m.MarketsUpserted, m.MarketOutcomesUpserted,
		m.MarketsSoftDeleted, m.MarketsPurged, m.MarketsResumed,
		m.TradesUpserted, m.TradesDuplicatesSkipped, m.TradersUpserted,
		m.BackfillPagesFetched, m.BackfillRunsTotal,
		m.StatsSummariesSent, m.StatsSummaryErrors,
		m.SignalReportsSent, m.TelegramReactions, m.AlertOutcomes,
		m.AlertRealizedEdge,
		m.AlertWeightedSuccessTotal, m.AlertWeightedResolvedTotal,
		m.AlertCalibrationTotal,
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
