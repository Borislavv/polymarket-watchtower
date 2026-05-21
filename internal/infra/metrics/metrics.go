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

	// --- AI analysis -----------------------------------------------
	// AIAnalysisRequests counts every analyzer call by terminal
	// status (ok / skipped / error). Useful to graph the actionable
	// hit-rate of the AI layer.
	AIAnalysisRequests *prometheus.CounterVec // status
	// AIAnalysisTokens by kind = prompt | completion.
	AIAnalysisTokens *prometheus.CounterVec // kind
	// AIAnalysisCost in USD, summed across all calls.
	AIAnalysisCost prometheus.Counter
	// AIAnalysisSkipped by reason: no_api_key / rate_limited /
	// daily_budget_exhausted / refresh_skipped.
	AIAnalysisSkipped *prometheus.CounterVec // reason
	// AIAnalysisLatency wall-clock seconds end-to-end.
	AIAnalysisLatency prometheus.Histogram

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

	// --- Market intelligence worker ---
	// MarketIntelligenceSkipped: labelled by reason (empty_report,
	// duplicate_period, ai_unavailable, ai_budget_denied). Increments
	// every time the 2h scout report is suppressed without Telegram
	// delivery. With v9.7 fallback wiring this counter should drop
	// to ~zero for ai_unavailable in steady state — the report now
	// ships a deterministic fallback when the AI times out.
	MarketIntelligenceSkipped *prometheus.CounterVec

	// MarketIntelAITimeout: per-AI-call timeout count for the 2h
	// scout. Differs from the broader AIRequestErrors metric in that
	// it ONLY counts CategoryTimeout failures (the noisy class we
	// retry-once on). A persistent climb means the prompt is too
	// heavy for the configured timeout.
	MarketIntelAITimeout prometheus.Counter

	// MarketIntelAIFallbackSent: number of reports that shipped the
	// deterministic fallback (markets / annotations / links) WITHOUT
	// an AI summary, by reason (timeout | retry_exhausted |
	// rate_limited | quota_exceeded | other). Visibility for the
	// silent-blindness fix in v9.7.
	MarketIntelAIFallbackSent *prometheus.CounterVec

	// MarketIntelLinksRendered: per-kind link counter so an operator
	// can verify the link-rendering pass actually emitted entries.
	// kind ∈ {event, market, category, grafana}. The renderer
	// increments after sanitizeLinkURL passes; broken links never
	// reach the metric.
	MarketIntelLinksRendered *prometheus.CounterVec

	// MarketIntelSourceLinksRendered: total annotation source links
	// rendered across all sent reports.
	MarketIntelSourceLinksRendered prometheus.Counter

	// AIRetries: marketintel retry-once-on-timeout instrumentation
	// (surface=market_intelligence; reason=timeout). Extra surfaces
	// can register entries here as they adopt the same retry policy.
	AIRetries *prometheus.CounterVec

	// AITimeoutTotal: per-surface timeout counter. Differs from
	// AIRequestErrors{reason=timeout} in that it captures the typed
	// CategoryTimeout cleanly and is the dashboard primary signal for
	// "the model is timing out on this surface".
	AITimeoutTotal *prometheus.CounterVec

	// AILatencySeconds: end-to-end latency of one AI call by surface,
	// including retry waits.
	AILatencySeconds *prometheus.HistogramVec

	// AIRequestErrors: labelled by kind (alert_note, market_intelligence,
	// outcome_postmortem) and reason (analyzer_error, budget_exhausted,
	// rate_limited, timeout). The single AI-failure visibility metric.
	AIRequestErrors *prometheus.CounterVec
	// AIAnalysisPersisted: incremented for every successful row landed
	// in polymarket_alert_analyses or polymarket_market_intelligence_reports.
	// Labelled by target_kind (alert | market_intelligence | outcome).
	AIAnalysisPersisted *prometheus.CounterVec
	// AIAnalysisRejected: incremented when AI output is rejected by
	// sanity checks (empty / provider-error-JSON). The v8.1 relaxation
	// removed structural validation, so this counter should stay near
	// zero in normal operation — a sudden rise means a provider
	// regression or a prompt shift.
	AIAnalysisRejected *prometheus.CounterVec
	// AIQuotaExceeded: 429 with insufficient_quota. Operator action
	// (billing) required; retry never helps.
	AIQuotaExceeded *prometheus.CounterVec

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

	// --- Event page narrative (Polymarket /event/<slug>.json) ---
	// EventPageFetch: fetch attempts labelled by status
	// (success / failed / persist_failed).
	EventPageFetch *prometheus.CounterVec
	// EventPageBuildIDChanges: incremented whenever the resolver
	// observes a NEW buildId (Polymarket Vercel deploy rotated).
	EventPageBuildIDChanges prometheus.Counter
	// EventPageAnnotations: total annotations parsed across all
	// fetches. Operator gauge for narrative volume.
	EventPageAnnotations prometheus.Counter
	// EventPageContextUsed: labelled by target_kind
	// (alert / market_intelligence / outcome) — increments every
	// time a non-empty event page context lands in an AI prompt.
	EventPageContextUsed *prometheus.CounterVec
	// EventPageAlerts: labelled by status — fires of the optional
	// event-page review worker (PART 8, currently scaffold only).
	EventPageAlerts *prometheus.CounterVec
	// EventPageLagCandidates: counts related-market lag flags
	// emitted by the lag detector (PART 9, currently scaffold).
	EventPageLagCandidates prometheus.Counter
	// EventPageFetchLatency: end-to-end fetch + parse latency.
	EventPageFetchLatency prometheus.Histogram
	// EventPageParseFailures: one increment per recoverable per-field
	// drift the parser had to fall through (labelled by JSON path).
	// Operator-actionable: a sustained climb on a specific field
	// means Polymarket changed encoding upstream.
	EventPageParseFailures *prometheus.CounterVec
	// EventPagePartialParse: counts whole-fetch outcomes where at
	// least one section recorded a parse warning. Pair with
	// EventPageParseFailures to see "how many fetches" vs "how many
	// fields".
	EventPagePartialParse prometheus.Counter
	// EventPageMarketParse: per-market parse outcome, labelled by
	// status ("ok" | "skipped"). A non-zero "skipped" rate means we
	// dropped at least one drifted market row.
	EventPageMarketParse *prometheus.CounterVec

	// --- v10.5 redirect + canonical-slug + stale-context metrics ---
	EventPageRedirects        *prometheus.CounterVec // status
	EventPageRedirectFailures *prometheus.CounterVec // reason
	EventPageBuildIDRefresh   *prometheus.CounterVec // reason
	EventPageSlugAlias        prometheus.Counter
	EventPageContextStale     *prometheus.CounterVec // reason

	// --- AI budget governance (single-process daily caps) ---
	// AIBudgetCharged: cumulative USD charged per bucket (today).
	AIBudgetCharged *prometheus.CounterVec
	// AIBudgetSpent: current per-bucket spend (USD) today, gauge.
	// Resets to 0 at UTC midnight.
	AIBudgetSpent *prometheus.GaugeVec
	// AIBudgetGlobalSpent: current global spend (USD) today, gauge.
	AIBudgetGlobalSpent prometheus.Gauge
	// AIBudgetDenied: denial counter labelled by bucket + reason
	// (bucket_exhausted | global_exhausted). A sustained non-zero
	// value on a high-priority bucket means caps are too tight.
	AIBudgetDenied *prometheus.CounterVec

	// --- v10.0 Prediction Creation worker ---
	// PredictionCreationRuns: per-cycle outcome counter (status:
	// ok | empty | candidates_failed | daily_cap_reached |
	// all_deduped | ai_disabled | no_picks).
	PredictionCreationRuns *prometheus.CounterVec
	// PredictionCreationCandidates: cumulative deterministic
	// shortlist size across cycles. Pair with Created to see
	// shortlist-to-thesis conversion.
	PredictionCreationCandidates prometheus.Counter
	// PredictionCreationCreated: predictions persisted, labelled
	// by category for spend / coverage analysis.
	PredictionCreationCreated *prometheus.CounterVec
	// PredictionCreationAIRequests: AI call outcomes labelled by
	// status (ranker_ok | ranker_failed | creator_ok | creator_failed).
	PredictionCreationAIRequests *prometheus.CounterVec
	// PredictionCreationAISkipped: AI calls denied by the budget
	// governor or gating layer, labelled by reason.
	PredictionCreationAISkipped *prometheus.CounterVec
	// PredictionCreationTelegram: send outcomes labelled by status
	// (sent | failed).
	PredictionCreationTelegram *prometheus.CounterVec
	// PredictionCreationLatency: end-to-end Tick duration.
	PredictionCreationLatency prometheus.Histogram

	// --- v10.1 Telegram/quality polish (PART 8) ---
	// PredictionCreationTelegramSkipped: Telegram sends suppressed
	// by the v10.1 gates, labelled by reason
	// (startup_suppressed | cooldown | max_per_run_reached |
	//  low_quality).
	PredictionCreationTelegramSkipped *prometheus.CounterVec
	// PredictionCreationTelegramSent: successful Telegram sends.
	PredictionCreationTelegramSent prometheus.Counter
	// PredictionCreationDedupeSkipped: dedupe outcomes from the
	// pre-AI filter, labelled by reason
	// (active_prediction | dedupe_window | low_interest |
	//  neutral_low_value).
	PredictionCreationDedupeSkipped *prometheus.CounterVec
	// PredictionCreationQualityGate: post-AI gate outcomes
	// (ok | low_confidence | low_summary | neutral_no_signal | no_signal).
	PredictionCreationQualityGate *prometheus.CounterVec
	// PredictionSchedulerStartupSuppressed: cross-worker counter
	// for "this scheduler suppressed Telegram on first cycle"
	// (currently emitted by the creation worker).
	PredictionSchedulerStartupSuppressed *prometheus.CounterVec
	// PredictionMessageChunks: count of Telegram chunks shipped per
	// surface (prediction_creation | prediction_evolution).
	// Multi-chunk implies the safe-split path fired.
	PredictionMessageChunks *prometheus.CounterVec

	// --- v10.2 prediction feedback (PART 4) ---
	// PredictionFeedbackRuns: per-cycle outcome counter
	// (ok | empty | failed).
	PredictionFeedbackRuns *prometheus.CounterVec
	// PredictionFeedbackProcessed: per-prediction outcome counter
	// (ok | market_lookup_failed | outcome_token_unknown | upsert_failed).
	PredictionFeedbackProcessed *prometheus.CounterVec
	// PredictionFeedbackHorizons: cumulative measurements by
	// horizon label (1h | 6h | 24h).
	PredictionFeedbackHorizons *prometheus.CounterVec
	// OutcomeMapping outcomes for telemetry on the v10.2 mapper.
	OutcomeMapping        *prometheus.CounterVec
	OutcomeMappingUnknown *prometheus.CounterVec

	// --- v10.3 worker overlap + cycle metrics ---
	// WorkerCycleDuration: per-worker tick wall-clock latency.
	// Labelled by worker name so the operator can see "creation
	// took 45s vs interval 30m" at a glance.
	WorkerCycleDuration *prometheus.HistogramVec
	// WorkerCycleSkipped: tick skipped because the previous tick
	// is still running (overlap=true). label: worker, reason.
	WorkerCycleSkipped *prometheus.CounterVec
	// WorkerCycleItems: count of items the worker processed in a
	// cycle, labelled by worker + status (ok | skipped | failed).
	WorkerCycleItems *prometheus.CounterVec

	// --- v10.3 AI cost + preflight ---
	// AIPromptChars: histogram of compacted prompt sizes per
	// surface. Operator-actionable: drift on prediction_creation
	// past the configured cap = compaction is firing.
	AIPromptChars *prometheus.HistogramVec
	// AICompactions: count of prompts the preflight had to compact,
	// labelled by surface + reason (chars_cap | output_cap).
	AICompactions *prometheus.CounterVec
	// AISurfaceSkipped: AI calls the preflight skipped after
	// compaction still left them over-cap, labelled by surface +
	// reason.
	AISurfaceSkipped *prometheus.CounterVec
	// AISurfaceEstimatedCost: pre-flight cost estimate counter,
	// labelled by surface. Pair with the real charged total to see
	// estimation drift.
	AISurfaceEstimatedCost *prometheus.CounterVec

	// --- v10.3 prediction archival + evaluation ---
	PredictionArchived   *prometheus.CounterVec
	PredictionStaled     *prometheus.CounterVec
	PredictionEvaluation *prometheus.CounterVec

	// --- v10.4 WebSocket realtime ingestion ---
	WSConnected           prometheus.Gauge
	WSReconnects          prometheus.Counter
	WSSubscriptions       prometheus.Gauge
	WSEvents              *prometheus.CounterVec
	WSDecodeErrors        *prometheus.CounterVec
	WSEventsDropped       *prometheus.CounterVec
	WSBufferDepth         prometheus.Gauge
	WSLastEventAgeSeconds prometheus.Gauge
	WSGapRecoveries       *prometheus.CounterVec
	WSReconcileDuration   prometheus.Histogram
	WSSubscriptionRefresh *prometheus.CounterVec
	RealtimeWorkEnqueued  *prometheus.CounterVec
	RealtimeWorkClaimed   *prometheus.CounterVec

	// --- v9.6 Political-Catalyst Intelligence importer ---
	// EventCatalystImporterRuns: importer cycle outcomes, labelled
	// by status (ok / empty / partial / failed).
	EventCatalystImporterRuns *prometheus.CounterVec
	// EventCatalystImporterSelected: cumulative unique event slugs
	// the selection step shortlisted across all cycles.
	EventCatalystImporterSelected prometheus.Counter
	// EventCatalystImporterProcessed: per-event outcomes within a
	// cycle (ok / fetch_failed / ai_failed / ai_skipped /
	// ai_disabled). High failure rates here are operator-actionable.
	EventCatalystImporterProcessed *prometheus.CounterVec
	// EventCatalystAIRequests: AI extraction calls, by status
	// (ok / skipped / failed).
	EventCatalystAIRequests *prometheus.CounterVec
	// EventCatalystUpserted: per-row outcomes (status, catalyst_type).
	EventCatalystUpserted *prometheus.CounterVec
	// EventCatalystImportLatency: end-to-end Tick latency.
	EventCatalystImportLatency prometheus.Histogram
	// EventCatalystBlockedAlerts: counts Telegram alerts stamped
	// with a Blocked Alert block by the alertsender.
	EventCatalystBlockedAlerts prometheus.Counter

	// --- v9.7 Annotation rendering + ranking + daily intel ---
	// AlertAnnotationBlocks: counts alerts where the annotation
	// stamper ran, labelled by status (rendered / empty).
	AlertAnnotationBlocks *prometheus.CounterVec
	// MarketIntelAnnotationRankingRequests: AI ranking calls in
	// the 2h marketintel cycle, by status (ok / skipped / failed).
	MarketIntelAnnotationRankingRequests *prometheus.CounterVec
	// MarketIntelAnnotationsSelected: cumulative ranked annotations
	// selected across 2h cycles.
	MarketIntelAnnotationsSelected prometheus.Counter
	// DailyPoliticalIntelReports: daily-report outcomes, by status
	// (sent / skipped / failed / ai_failed).
	DailyPoliticalIntelReports *prometheus.CounterVec
	// DailyPoliticalIntelMarketsSelected: cumulative markets the
	// daily worker shortlisted.
	DailyPoliticalIntelMarketsSelected prometheus.Counter
	// DailyPoliticalIntelAnnotations: cumulative annotations the
	// daily worker passed to the AI prompt.
	DailyPoliticalIntelAnnotations prometheus.Counter
	// DailyPoliticalIntelAILatency: AI generation latency.
	DailyPoliticalIntelAILatency prometheus.Histogram

	// --- v9.8 Intelligence Hardening ---
	// EventFlowSummaryLoad: per-call status of the deterministic
	// event-flow aggregator. Labels: status=ok|empty|alerts_failed.
	EventFlowSummaryLoad *prometheus.CounterVec
	// EventFlowSummaryEmpty: counts events where the loader found
	// zero alerts + zero trades in the lookback. Useful for
	// "intelligence dark" detection.
	EventFlowSummaryEmpty prometheus.Counter
	// RepricingSignals: deterministic repricing-signal writes,
	// labelled by status (computed/failed) and flow_timing.
	RepricingSignals *prometheus.CounterVec
	// MarketPredictionStateTransitions: state machine transitions,
	// labelled by from→to.
	MarketPredictionStateTransitions *prometheus.CounterVec
	// MarketPredictionMatches: per-match outcomes labelled by
	// direction alignment (aligned/contradict/neutral).
	MarketPredictionMatches *prometheus.CounterVec
	// PredictionContextBlocks: counts per-prompt context block
	// usage (flow / repricing / state / matched_alerts) and
	// status (rendered / empty).
	PredictionContextBlocks *prometheus.CounterVec

	// --- v9.9 Prediction Evolution Worker ---
	// PredictionEvolutionRuns: cycle-level outcome counter.
	PredictionEvolutionRuns *prometheus.CounterVec
	// PredictionEvolutionSelected: cumulative predictions
	// shortlisted by the selection query.
	PredictionEvolutionSelected prometheus.Counter
	// PredictionEvolutionProcessed: per-prediction outcomes within
	// a cycle (ok / failed / skipped).
	PredictionEvolutionProcessed *prometheus.CounterVec
	// PredictionEvolutionStateChanges: prediction state transitions
	// captured by the evolution worker (from → to). Distinct from
	// MarketPredictionStateTransitions which counts ALL transitions
	// across the system.
	PredictionEvolutionStateChanges *prometheus.CounterVec
	// PredictionEvolutionAIRequests: thesis-refresh AI calls
	// (ok / failed).
	PredictionEvolutionAIRequests *prometheus.CounterVec
	// PredictionEvolutionAISkipped: AI calls the gating layer
	// dropped (reason label).
	PredictionEvolutionAISkipped *prometheus.CounterVec
	// PredictionEvolutionTelegram: Telegram deliveries per cycle
	// (sent / failed / suppressed_cooldown / skipped_<reason>).
	PredictionEvolutionTelegram *prometheus.CounterVec

	// --- v10.7 AI sentinel + gating metrics ---
	// AISentinelTotal: count of sentinel codes returned across every
	// surface (prediction_evolution / market_intel / prediction_creation
	// / daily_intel). Labels: surface, code.
	AISentinelTotal *prometheus.CounterVec
	// PredictionSentinelSuppressed: per-code count of prediction
	// updates suppressed because the AI returned a sentinel.
	PredictionSentinelSuppressed *prometheus.CounterVec
	// AIPrecallSkipped: pre-call gating decisions (news_unchanged /
	// semantic_cooldown / stale_context / no_price_move).
	AIPrecallSkipped *prometheus.CounterVec
	// DedupeSuppressed: duplicate output suppressed (by surface +
	// reason: semantic_cooldown / period_dedupe / news_unchanged).
	DedupeSuppressed *prometheus.CounterVec
	// MarketIntelNoEdgeSuppressed: marketintel reports persisted but
	// NOT sent because the AI/quality gate determined "no edge".
	MarketIntelNoEdgeSuppressed prometheus.Counter
	// AIWorkflowAntiPattern: increments when the orchestrator
	// detects a 5+1-style anti-pattern (per-item AI call + aggregator).
	AIWorkflowAntiPattern *prometheus.CounterVec
	// NewsFingerprintChanged: per-surface count of fingerprint flips
	// (signals fresh news arrived for an event).
	NewsFingerprintChanged *prometheus.CounterVec
	// NewsFingerprintUnchanged: per-surface count of fingerprint
	// matches (no fresh news; gating suppresses AI by default).
	NewsFingerprintUnchanged *prometheus.CounterVec

	// --- v10.8 concentration / escalation gate ---
	// ConcentrationSuppressed: alerts dropped by the per-event /
	// per-wallet gate, labelled by reason
	// (wallet_escalation_failed / event_concentration_cap).
	ConcentrationSuppressed *prometheus.CounterVec
	// PredictionEvolutionLatency: end-to-end Tick duration.
	PredictionEvolutionLatency prometheus.Histogram
	// PredictionEvolutionDecay: decay applications, labelled by
	// the state the prediction is in when decayed.
	PredictionEvolutionDecay *prometheus.CounterVec
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

	m.AIAnalysisRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai_analysis", Name: "requests_total",
		Help: "AI analyzer calls by terminal status (ok|skipped|error).",
	}, []string{"status"})
	m.AIAnalysisTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai_analysis", Name: "tokens_total",
		Help: "Tokens consumed by the AI analyzer, split by prompt/completion.",
	}, []string{"kind"})
	m.AIAnalysisCost = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai_analysis", Name: "estimated_cost_usd_total",
		Help: "Running total of estimated AI spend in USD. Approximate — reconcile against the analysis tables for ground truth.",
	})
	m.AIAnalysisSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai_analysis", Name: "skipped_total",
		Help: "AI calls deliberately skipped by policy. reason: no_api_key | rate_limited | daily_budget_exhausted | refresh_skipped | …",
	}, []string{"reason"})
	m.AIAnalysisLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "ai_analysis", Name: "latency_seconds",
		Help:    "End-to-end wall-clock latency of an AI analyzer call.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 30},
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

	m.MarketIntelligenceSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "market_intelligence", Name: "skipped_total",
		Help: "Periodic 2h scout reports suppressed without Telegram delivery, by reason (empty_report, duplicate_period, ai_unavailable, ai_budget_denied).",
	}, []string{"reason"})

	m.MarketIntelAITimeout = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "marketintel", Name: "ai_timeout_total",
		Help: "Number of marketintel AI calls that tripped a context_deadline_exceeded / typed timeout. Surfaces the noisy failure mode we retry-once on.",
	})

	m.MarketIntelAIFallbackSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "marketintel", Name: "ai_fallback_sent_total",
		Help: "Reports delivered with the deterministic fallback (no AI summary) instead of being suppressed. Labelled by reason.",
	}, []string{"reason"})

	m.MarketIntelLinksRendered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "marketintel", Name: "links_rendered_total",
		Help: "Telegram links rendered in marketintel reports, by kind (event | market | category | grafana | source).",
	}, []string{"kind"})

	m.MarketIntelSourceLinksRendered = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "marketintel", Name: "source_links_rendered_total",
		Help: "Annotation source links rendered in marketintel reports. Counter; matches the per-link {kind=source} entries above.",
	})

	m.AIRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "retries_total",
		Help: "AI request retries by surface + reason (timeout). Bumped exactly once per retry-once attempt; does NOT include the original call.",
	}, []string{"surface", "reason"})

	m.AITimeoutTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "timeout_total",
		Help: "Typed CategoryTimeout failures per AI surface. Distinct from request_errors_total in that it ONLY counts timeouts.",
	}, []string{"surface"})

	m.AILatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "latency_seconds",
		Help:    "End-to-end AI call latency by surface (includes retry waits).",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 9), // 0.5s .. ~256s
	}, []string{"surface"})

	m.AIRequestErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "request_errors_total",
		Help: "AI requests that failed before producing usable output, by kind (alert_note, market_intelligence, outcome_postmortem) and reason.",
	}, []string{"kind", "reason"})

	m.AIAnalysisPersisted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "analysis_persisted_total",
		Help: "AI answers landed in polymarket_alert_analyses / polymarket_market_intelligence_reports. target_kind: alert | market_intelligence | outcome.",
	}, []string{"target_kind"})

	m.AIAnalysisRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "analysis_rejected_total",
		Help: "AI output rejected by sanity checks (empty_text | provider_error_text). v8.1 removed structural validation; a non-zero rate here means a provider regression.",
	}, []string{"target_kind", "reason"})

	m.AIQuotaExceeded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "quota_exceeded_total",
		Help: "Provider returned HTTP 429 with insufficient_quota. Operator action required; retry is useless.",
	}, []string{"provider", "model"})

	// Event page narrative (Polymarket /event/<slug>.json).
	m.EventPageFetch = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "fetch_total",
		Help: "Polymarket event page fetch attempts, by status (success / failed / persist_failed).",
	}, []string{"status"})
	m.EventPageRedirects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "eventpage", Name: "redirects_total",
		Help: "Polymarket event page HTTP redirects observed, labelled by status code (307 is the v10.5 hot path).",
	}, []string{"status"})
	m.EventPageRedirectFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "eventpage", Name: "redirect_failures_total",
		Help: "Redirect handling failures, by reason (missing_location, loop_or_cap, unsupported_target, html_no_next_data, non_200).",
	}, []string{"reason"})
	m.EventPageBuildIDRefresh = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "eventpage", Name: "buildid_refresh_total",
		Help: "Forced buildId refreshes triggered by the fetch path, by reason (stale_build_id, json_parse_failed).",
	}, []string{"reason"})
	m.EventPageSlugAlias = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "eventpage", Name: "slug_alias_total",
		Help: "Canonical-slug aliases recorded (original → canonical mappings persisted).",
	})
	m.EventPageContextStale = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "eventpage", Name: "context_stale_total",
		Help: "AI / Telegram surfaces consumed stale event-page context (live fetch failed; cached snapshot served instead), by reason.",
	}, []string{"reason"})
	m.EventPageBuildIDChanges = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "build_id_changes_total",
		Help: "Resolver observed a NEW Polymarket Next.js buildId (Vercel deploy rotated).",
	})
	m.EventPageAnnotations = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "annotations_total",
		Help: "Total annotations parsed across all event page fetches.",
	})
	m.EventPageContextUsed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "context_used_total",
		Help: "Event page context blocks injected into AI prompts, by target_kind (alert / market_intelligence / outcome).",
	}, []string{"target_kind"})
	m.EventPageAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "alerts_total",
		Help: "Event-page-review alert firings, by status (sent / skipped / failed). Scaffold; populated when the review worker is enabled.",
	}, []string{"status"})
	m.EventPageLagCandidates = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "lag_candidates_total",
		Help: "Related-market lag flags emitted by the lag detector. Scaffold; populated when the detector is enabled.",
	})
	m.EventPageFetchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "fetch_latency_seconds",
		Help:    "End-to-end latency of one event page fetch + parse.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 9),
	})
	m.EventPageParseFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "parse_failures_total",
		Help: "Recoverable per-field parse drifts, labelled by JSON path. Sustained climb on a specific field indicates upstream Polymarket encoding change.",
	}, []string{"field"})
	m.EventPagePartialParse = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "partial_parse_total",
		Help: "Fetches that produced at least one parse warning. Paired with parse_failures_total to see fetch-level vs field-level drift.",
	})
	m.EventPageMarketParse = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_page", Name: "market_parse_total",
		Help: "Per-market parse outcomes (ok / skipped). A non-zero skipped rate indicates one or more markets in the event were structurally unreadable and dropped.",
	}, []string{"status"})

	// AI budget governance (single-process daily caps)
	m.AIBudgetCharged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai_budget", Name: "charged_usd_total",
		Help: "Cumulative USD charged per AI bucket today. Resets at UTC midnight by counter rollover; the gauge below carries the live value.",
	}, []string{"bucket"})
	m.AIBudgetSpent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "ai_budget", Name: "spent_usd",
		Help: "Per-bucket spend today (USD). Resets to 0 at UTC midnight.",
	}, []string{"bucket"})
	m.AIBudgetGlobalSpent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "ai_budget", Name: "global_spent_usd",
		Help: "Global AI spend today across all buckets (USD). Resets to 0 at UTC midnight.",
	})
	m.AIBudgetDenied = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai_budget", Name: "denied_total",
		Help: "AI calls denied by the budget governor, labelled by bucket + reason (bucket_exhausted | global_exhausted). A persistent non-zero on alerts means the cap is too tight.",
	}, []string{"bucket", "reason"})

	// v10.0 Prediction Creation worker
	m.PredictionCreationRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "runs_total",
		Help: "Per-cycle outcome counter for the prediction creation worker.",
	}, []string{"status"})
	m.PredictionCreationCandidates = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "candidates_total",
		Help: "Cumulative size of the deterministic shortlist the worker handed the AI ranker.",
	})
	m.PredictionCreationCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "created_total",
		Help: "Predictions persisted, labelled by category.",
	}, []string{"category"})
	m.PredictionCreationAIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "ai_requests_total",
		Help: "AI call outcomes (ranker_ok | ranker_failed | creator_ok | creator_failed).",
	}, []string{"status"})
	m.PredictionCreationAISkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "ai_skipped_total",
		Help: "AI calls the gating layer or budget governor skipped, labelled by reason.",
	}, []string{"reason"})
	m.PredictionCreationTelegram = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "telegram_total",
		Help: "Send outcomes (sent | failed) for the PREDICTION CREATED Telegram body.",
	}, []string{"status"})
	m.PredictionCreationLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "latency_seconds",
		Help:    "End-to-end Tick duration of the prediction creation worker.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 9),
	})

	// v10.1 Telegram + quality + scheduler polish (PART 8).
	m.PredictionCreationTelegramSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "telegram_skipped_total",
		Help: "Telegram sends suppressed by the gates: startup_suppressed | cooldown | max_per_run_reached | low_quality.",
	}, []string{"reason"})
	m.PredictionCreationTelegramSent = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "telegram_sent_total",
		Help: "Successful Telegram sends from the creation worker (one increment per prediction, not per chunk).",
	})
	m.PredictionCreationDedupeSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "dedupe_skipped_total",
		Help: "Pre-AI dedupe outcomes: active_prediction | dedupe_window | low_interest | neutral_low_value.",
	}, []string{"reason"})
	m.PredictionCreationQualityGate = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_creation", Name: "quality_gate_total",
		Help: "Post-AI quality gate outcomes: ok | low_confidence | low_summary | neutral_no_signal | no_signal.",
	}, []string{"result"})
	m.PredictionSchedulerStartupSuppressed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_scheduler", Name: "startup_suppressed_total",
		Help: "First-cycle Telegram suppression per worker (currently prediction_creation).",
	}, []string{"worker"})
	m.PredictionMessageChunks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction", Name: "message_chunks_total",
		Help: "Telegram chunks shipped per surface (prediction_creation | prediction_evolution). Multi-chunk implies the safe-split path fired.",
	}, []string{"surface"})

	// v10.2 prediction feedback + outcome mapping.
	m.PredictionFeedbackRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_feedback", Name: "runs_total",
		Help: "Per-cycle outcome counter for the prediction-feedback worker.",
	}, []string{"status"})
	m.PredictionFeedbackProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_feedback", Name: "processed_total",
		Help: "Per-prediction feedback outcomes: ok | market_lookup_failed | outcome_token_unknown | upsert_failed.",
	}, []string{"status"})
	m.PredictionFeedbackHorizons = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_feedback", Name: "horizons_total",
		Help: "Cumulative feedback measurements written per horizon (1h | 6h | 24h).",
	}, []string{"horizon"})
	m.OutcomeMapping = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "outcome_mapping", Name: "total",
		Help: "Outcome mapping outcomes labelled by status (ok | unknown).",
	}, []string{"status"})
	m.OutcomeMappingUnknown = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "outcome_mapping", Name: "unknown_total",
		Help: "Outcome-mapping unknowns labelled by reason code (unknown_condition_id | unknown_token_id | label_not_found | …).",
	}, []string{"reason"})

	// v10.3 worker overlap + cycle metrics.
	m.WorkerCycleDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "worker", Name: "cycle_duration_seconds",
		Help:    "Per-worker tick wall-clock latency. Use to see when a Tick is approaching its Interval and tune timeouts.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
	}, []string{"worker"})
	m.WorkerCycleSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "worker", Name: "cycle_skipped_total",
		Help: "Worker ticks skipped — labelled by worker + reason (overlap | timeout | disabled).",
	}, []string{"worker", "reason"})
	m.WorkerCycleItems = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "worker", Name: "cycle_items_total",
		Help: "Per-worker per-item outcome counter. Workers wire their own status taxonomy here for the operator dashboard.",
	}, []string{"worker", "status"})

	// v10.3 AI cost + preflight.
	m.AIPromptChars = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "prompt_chars",
		Help:    "Compacted prompt size per AI surface (alert / catalyst / prediction_create / prediction_evolution / daily_intel / market_intel).",
		Buckets: prometheus.ExponentialBuckets(1000, 2, 8),
	}, []string{"surface"})
	m.AICompactions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "compactions_total",
		Help: "Prompts the preflight compacted, labelled by surface + reason (chars_cap | output_cap).",
	}, []string{"surface", "reason"})
	m.AISurfaceSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "surface_skipped_total",
		Help: "AI calls the preflight skipped after compaction still left them over the cap, labelled by surface + reason.",
	}, []string{"surface", "reason"})
	m.AISurfaceEstimatedCost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "estimated_cost_usd_total",
		Help: "Pre-flight cost estimate counter labelled by surface. Compare against ai_budget_charged_usd_total to see estimation drift.",
	}, []string{"surface"})

	// v10.3 prediction archival + evaluation.
	m.PredictionArchived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_archival", Name: "archived_total",
		Help: "Predictions archived by the v10.3 worker, labelled by terminal state + reason.",
	}, []string{"state", "reason"})
	m.PredictionStaled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_archival", Name: "staled_total",
		Help: "Predictions the worker flipped to state=stale, labelled by reason.",
	}, []string{"reason"})
	m.PredictionEvaluation = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evaluation", Name: "total",
		Help: "Prediction evaluations written, labelled by classifier output + horizon.",
	}, []string{"evaluation", "horizon"})

	// v10.4 WebSocket realtime ingestion.
	m.WSConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "connected",
		Help: "1 when the Polymarket CLOB WS client is connected, 0 otherwise.",
	})
	m.WSReconnects = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "reconnects_total",
		Help: "Cumulative WS reconnect attempts.",
	})
	m.WSSubscriptions = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "subscriptions_total",
		Help: "Current count of subscribed CLOB token ids on the live connection.",
	})
	m.WSEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "events_total",
		Help: "WS messages received, labelled by normalised event_type (book / price_change / last_trade_price / best_bid_ask / tick_size_change / market_resolved / heartbeat / unknown).",
	}, []string{"type"})
	m.WSDecodeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "decode_errors_total",
		Help: "Decode failures on inbound WS messages, labelled by event_type (where parseable).",
	}, []string{"type"})
	m.WSEventsDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "events_dropped_total",
		Help: "Events dropped by the bounded output channel / drop policy, labelled by reason + event_type.",
	}, []string{"reason", "type"})
	m.WSBufferDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "buffer_depth",
		Help: "Current depth of the WS output channel.",
	})
	m.WSLastEventAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "last_event_age_seconds",
		Help: "Seconds since the last inbound WS message. Used for the health-stale check.",
	})
	m.WSGapRecoveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "gap_recoveries_total",
		Help: "Per-condition gap-recovery sweep outcomes (ok / no_trades / partial / failed).",
	}, []string{"status"})
	m.WSReconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "reconcile_duration_seconds",
		Help:    "End-to-end duration of one reconciliation sweep across the subscribed set.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
	})
	m.WSSubscriptionRefresh = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ws", Name: "subscription_refresh_total",
		Help: "Subscription-set refresh cycles, labelled by outcome (ok / unchanged / failed).",
	}, []string{"status"})
	m.RealtimeWorkEnqueued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "realtime", Name: "work_enqueued_total",
		Help: "polymarket_realtime_work_queue inserts, labelled by reason (price_move | book_change | trade_seen | market_status | gap_recovered).",
	}, []string{"reason"})
	m.RealtimeWorkClaimed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "realtime", Name: "work_claimed_total",
		Help: "Realtime work-queue claim outcomes, labelled by reason + status (ok / failed).",
	}, []string{"reason", "status"})

	// v9.6 Political-Catalyst Intelligence importer
	m.EventCatalystImporterRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst_importer", Name: "runs_total",
		Help: "Importer cycles, by status (ok / empty / partial / failed).",
	}, []string{"status"})
	m.EventCatalystImporterSelected = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst_importer", Name: "events_selected_total",
		Help: "Cumulative unique event slugs the selection step shortlisted.",
	})
	m.EventCatalystImporterProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst_importer", Name: "events_processed_total",
		Help: "Per-event outcomes within a cycle (ok / fetch_failed / ai_failed / ai_skipped / ai_disabled).",
	}, []string{"status"})
	m.EventCatalystAIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst", Name: "ai_requests_total",
		Help: "AI catalyst-extraction calls, by status (ok / skipped / failed).",
	}, []string{"status"})
	m.EventCatalystUpserted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst", Name: "upserted_total",
		Help: "Catalyst row writes by (status, catalyst_type). status=stale is emitted by the stale-marker path.",
	}, []string{"status", "type"})
	m.EventCatalystImportLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst_importer", Name: "import_latency_seconds",
		Help:    "End-to-end Tick latency for one importer cycle.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10),
	})
	m.EventCatalystBlockedAlerts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_catalyst", Name: "blocked_alerts_total",
		Help: "Telegram alerts stamped with a Blocked Alert block.",
	})

	// v9.7 Annotation rendering + ranking + daily intel
	m.AlertAnnotationBlocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "alert", Name: "annotation_blocks_total",
		Help: "Alerts where the annotation stamper ran, by status (rendered / empty).",
	}, []string{"status"})
	m.MarketIntelAnnotationRankingRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "market_intel", Name: "annotation_ranking_requests_total",
		Help: "AI annotation-ranking calls in the 2h marketintel cycle, by status (ok / skipped / failed).",
	}, []string{"status"})
	m.MarketIntelAnnotationsSelected = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "market_intel", Name: "annotations_selected_total",
		Help: "Cumulative ranked annotations selected across 2h cycles.",
	})
	m.DailyPoliticalIntelReports = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "daily_political_intel", Name: "reports_total",
		Help: "Daily-report outcomes, by status (sent / skipped / failed / ai_failed).",
	}, []string{"status"})
	m.DailyPoliticalIntelMarketsSelected = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "daily_political_intel", Name: "markets_selected_total",
		Help: "Cumulative markets the daily worker shortlisted.",
	})
	m.DailyPoliticalIntelAnnotations = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "daily_political_intel", Name: "annotations_total",
		Help: "Cumulative annotations the daily worker passed to the AI prompt.",
	})
	m.DailyPoliticalIntelAILatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "daily_political_intel", Name: "ai_latency_seconds",
		Help:    "Daily-report AI generation latency.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 9),
	})

	// v9.8 Intelligence Hardening
	m.EventFlowSummaryLoad = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_flow_summary", Name: "load_total",
		Help: "Per-call status of the deterministic event-flow aggregator (ok / empty / alerts_failed).",
	}, []string{"status"})
	m.EventFlowSummaryEmpty = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "event_flow_summary", Name: "empty_total",
		Help: "Events for which the loader found zero alerts AND zero trades.",
	})
	m.RepricingSignals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "repricing", Name: "signals_total",
		Help: "Deterministic repricing-signal writes, by status and flow_timing.",
	}, []string{"status", "flow_timing"})
	m.MarketPredictionStateTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "market_prediction", Name: "state_transitions_total",
		Help: "Prediction state machine transitions (from → to).",
	}, []string{"from", "to"})
	m.MarketPredictionMatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "market_prediction", Name: "matches_total",
		Help: "Alert/prediction match outcomes by direction alignment.",
	}, []string{"alignment"})
	m.PredictionContextBlocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_context", Name: "blocks_total",
		Help: "Per-prompt context block usage (block, status).",
	}, []string{"block", "status"})

	// v9.9 Prediction Evolution Worker
	m.PredictionEvolutionRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "runs_total",
		Help: "Evolution worker cycle outcomes (ok / failed / empty).",
	}, []string{"status"})
	m.PredictionEvolutionSelected = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "selected_total",
		Help: "Cumulative predictions shortlisted by the selection query.",
	})
	m.PredictionEvolutionProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "processed_total",
		Help: "Per-prediction outcomes within a cycle (ok / failed / skipped).",
	}, []string{"status"})
	m.PredictionEvolutionStateChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "state_changes_total",
		Help: "Prediction state transitions captured by the evolution worker (from → to).",
	}, []string{"from", "to"})
	m.PredictionEvolutionAIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "ai_requests_total",
		Help: "Thesis-refresh AI calls (ok / failed).",
	}, []string{"status"})
	m.PredictionEvolutionAISkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "ai_skipped_total",
		Help: "AI calls the gating layer dropped, by reason.",
	}, []string{"reason"})
	m.PredictionEvolutionTelegram = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "telegram_total",
		Help: "Telegram deliveries per cycle (sent / failed / suppressed_cooldown / skipped_<reason>).",
	}, []string{"status"})

	// v10.7 AI sentinel + gating metrics.
	m.AISentinelTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "sentinel_total",
		Help: "AI returned a sentinel code instead of analytical output, by surface + code.",
	}, []string{"surface", "code"})
	m.PredictionSentinelSuppressed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction", Name: "sentinel_suppressed_total",
		Help: "Prediction updates suppressed because the AI returned a sentinel code.",
	}, []string{"code"})
	m.AIPrecallSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "precall_skipped_total",
		Help: "AI calls skipped before the HTTP roundtrip, by surface + reason (news_unchanged / semantic_cooldown / stale_context / no_price_move / no_secondary_trigger).",
	}, []string{"surface", "reason"})
	m.DedupeSuppressed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "dedupe", Name: "suppressed_total",
		Help: "Duplicate outputs suppressed, by surface + reason.",
	}, []string{"surface", "reason"})
	m.MarketIntelNoEdgeSuppressed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "marketintel", Name: "no_edge_suppressed_total",
		Help: "Marketintel reports persisted but NOT sent because the AI/quality gate determined no edge.",
	})
	m.AIWorkflowAntiPattern = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "ai", Name: "workflow_anti_pattern_total",
		Help: "Detected 5+1-style anti-patterns (per-item AI + aggregator). Should stay zero.",
	}, []string{"workflow", "reason"})
	m.NewsFingerprintChanged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "news_fingerprint", Name: "changed_total",
		Help: "Per-surface count of news fingerprint flips (fresh news observed).",
	}, []string{"surface"})
	m.NewsFingerprintUnchanged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "news_fingerprint", Name: "unchanged_total",
		Help: "Per-surface count of fingerprint matches (no fresh news; AI gating suppresses).",
	}, []string{"surface"})

	// v10.8 concentration gate.
	m.ConcentrationSuppressed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "concentration", Name: "suppressed_total",
		Help: "Alerts dropped by the per-event / per-wallet concentration gate, by reason (wallet_escalation_failed / event_concentration_cap).",
	}, []string{"reason"})
	m.PredictionEvolutionLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "latency_seconds",
		Help:    "End-to-end Tick duration.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 9),
	})
	m.PredictionEvolutionDecay = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "watchtower", Subsystem: "prediction_evolution", Name: "decay_total",
		Help: "Decay applications by state.",
	}, []string{"state"})

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
		m.AIAnalysisRequests, m.AIAnalysisTokens, m.AIAnalysisCost, m.AIAnalysisSkipped, m.AIAnalysisLatency,
		m.MarketsUpserted, m.MarketOutcomesUpserted,
		m.MarketsSoftDeleted, m.MarketsPurged, m.MarketsResumed,
		m.TradesUpserted, m.TradesDuplicatesSkipped, m.TradersUpserted,
		m.BackfillPagesFetched, m.BackfillRunsTotal,
		m.StatsSummariesSent, m.StatsSummaryErrors,
		m.MarketIntelligenceSkipped, m.AIRequestErrors,
		m.MarketIntelAITimeout, m.MarketIntelAIFallbackSent,
		m.MarketIntelLinksRendered, m.MarketIntelSourceLinksRendered,
		m.AIRetries, m.AITimeoutTotal, m.AILatencySeconds,
		m.AIAnalysisPersisted, m.AIAnalysisRejected, m.AIQuotaExceeded,
		m.SignalReportsSent, m.TelegramReactions, m.AlertOutcomes,
		m.AlertRealizedEdge,
		m.AlertWeightedSuccessTotal, m.AlertWeightedResolvedTotal,
		m.AlertCalibrationTotal,
		m.EventPageFetch, m.EventPageBuildIDChanges, m.EventPageAnnotations,
		m.EventPageContextUsed, m.EventPageAlerts, m.EventPageLagCandidates,
		m.EventPageRedirects, m.EventPageRedirectFailures, m.EventPageBuildIDRefresh,
		m.EventPageSlugAlias, m.EventPageContextStale,
		m.EventPageFetchLatency,
		m.EventPageParseFailures, m.EventPagePartialParse, m.EventPageMarketParse,
		m.AIBudgetCharged, m.AIBudgetSpent, m.AIBudgetGlobalSpent, m.AIBudgetDenied,
		m.PredictionCreationRuns, m.PredictionCreationCandidates, m.PredictionCreationCreated,
		m.PredictionCreationAIRequests, m.PredictionCreationAISkipped, m.PredictionCreationTelegram,
		m.PredictionCreationLatency,
		m.PredictionCreationTelegramSkipped, m.PredictionCreationTelegramSent,
		m.PredictionCreationDedupeSkipped, m.PredictionCreationQualityGate,
		m.PredictionSchedulerStartupSuppressed, m.PredictionMessageChunks,
		m.PredictionFeedbackRuns, m.PredictionFeedbackProcessed, m.PredictionFeedbackHorizons,
		m.OutcomeMapping, m.OutcomeMappingUnknown,
		m.WorkerCycleDuration, m.WorkerCycleSkipped, m.WorkerCycleItems,
		m.AIPromptChars, m.AICompactions, m.AISurfaceSkipped, m.AISurfaceEstimatedCost,
		m.PredictionArchived, m.PredictionStaled, m.PredictionEvaluation,
		m.WSConnected, m.WSReconnects, m.WSSubscriptions, m.WSEvents,
		m.WSDecodeErrors, m.WSEventsDropped, m.WSBufferDepth,
		m.WSLastEventAgeSeconds, m.WSGapRecoveries, m.WSReconcileDuration,
		m.WSSubscriptionRefresh, m.RealtimeWorkEnqueued, m.RealtimeWorkClaimed,
		m.EventCatalystImporterRuns, m.EventCatalystImporterSelected,
		m.EventCatalystImporterProcessed, m.EventCatalystAIRequests,
		m.EventCatalystUpserted, m.EventCatalystImportLatency,
		m.EventCatalystBlockedAlerts,
		m.AlertAnnotationBlocks,
		m.MarketIntelAnnotationRankingRequests, m.MarketIntelAnnotationsSelected,
		m.DailyPoliticalIntelReports, m.DailyPoliticalIntelMarketsSelected,
		m.DailyPoliticalIntelAnnotations, m.DailyPoliticalIntelAILatency,
		m.EventFlowSummaryLoad, m.EventFlowSummaryEmpty,
		m.RepricingSignals, m.MarketPredictionStateTransitions,
		m.MarketPredictionMatches, m.PredictionContextBlocks,
		m.PredictionEvolutionRuns, m.PredictionEvolutionSelected,
		m.PredictionEvolutionProcessed, m.PredictionEvolutionStateChanges,
		m.PredictionEvolutionAIRequests, m.PredictionEvolutionAISkipped,
		m.PredictionEvolutionTelegram, m.PredictionEvolutionLatency,
		m.AISentinelTotal, m.PredictionSentinelSuppressed, m.AIPrecallSkipped,
		m.DedupeSuppressed, m.MarketIntelNoEdgeSuppressed, m.AIWorkflowAntiPattern,
		m.NewsFingerprintChanged, m.NewsFingerprintUnchanged,
		m.ConcentrationSuppressed,
		m.PredictionEvolutionDecay,
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
