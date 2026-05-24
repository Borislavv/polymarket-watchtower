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

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/accumulation"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/mmfilter"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/ownership"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/quietmarket"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/score"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/concentration"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/stagedinputs"
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

// TraderBaselineFetcher serves the wallet's full-history distribution to
// the per-trade scorer. Satisfied in production by
// internal/app/usecase/analytics/traderbaseline.Provider. When nil, the
// trader axis is disabled and scoring falls back to v1 behaviour
// (market-only multiplier).
type TraderBaselineFetcher interface {
	Stats(ctx context.Context, wallet string) (baseline.Stats, error)
}

// MMArbFilter decides whether a fired single-trade candidate looks like
// market-making or arbitrage activity that should be suppressed before
// emission. Satisfied by *mmfilter.Filter. nil disables suppression.
type MMArbFilter interface {
	Decide(ctx context.Context, wallet string, marketID int64, outcomeToken string) (mmfilter.Verdict, error)
}

// AccumulationLineFetcher returns the server-side roll-up of one wallet's
// recent same-side trades on a (market, outcome) bucket. Satisfied by
// *repository.TradeRepository.
type AccumulationLineFetcher interface {
	AccumulationLineSummary(ctx context.Context, q repository.AccumulationLineQuery) (repository.AccumulationLine, error)
}

// LastTradeBeforeFetcher returns the most recent traded_at for a
// (market, outcome) strictly before the supplied timestamp. Used by the
// quiet-market wake-up detector to compute idle gap. Satisfied by
// *repository.TradeRepository.
type LastTradeBeforeFetcher interface {
	LastTradedAtBefore(ctx context.Context, marketID int64, outcomeToken string, before time.Time) (time.Time, error)
}

// OwnershipSharesFetcher returns the trade-flow approximation of a
// wallet's position vs the outcome's total recorded BUY flow. Satisfied
// by *repository.TradeRepository.
type OwnershipSharesFetcher interface {
	OwnershipShares(ctx context.Context, q repository.OwnershipSharesQuery) (repository.OwnershipShares, error)
}

// TraderActivityFetcher returns the wallet's most-recent traded_at
// strictly before the supplied cutoff. Used by the dormant-wallet
// booster to compute idle duration. Satisfied by
// *repository.TradeRepository.TraderLastSeenBefore.
type TraderActivityFetcher interface {
	TraderLastSeenBefore(ctx context.Context, traderID int64, before time.Time) (time.Time, error)
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

// ConcentrationGate is the v10.8 per-event + per-wallet concentration
// gate. Satisfied by *concentration.Gate. nil disables.
type ConcentrationGate interface {
	Evaluate(ctx context.Context, in concentration.Candidate, history concentration.AlertHistory) (concentration.Decision, error)
}

// ConcentrationHistory is the seam the gate uses to look up recent
// alerts. Satisfied by *repository.AlertRepository — its
// ConcentrationAlert type maps 1:1 to concentration.Alert.
type ConcentrationHistory interface {
	RecentAlertsForEvent(ctx context.Context, eventSlug string, since time.Time) ([]repository.ConcentrationAlert, error)
	RecentAlertsForWallet(ctx context.Context, wallet, eventSlug string, since time.Time) ([]repository.ConcentrationAlert, error)
}

// concentrationHistoryAdapter projects repository.ConcentrationAlert
// rows onto concentration.Alert so the pure gate package never
// imports the repository package.
type concentrationHistoryAdapter struct {
	hist ConcentrationHistory
}

func (a concentrationHistoryAdapter) RecentAlertsForEvent(ctx context.Context, eventSlug string, since time.Time) ([]concentration.Alert, error) {
	rows, err := a.hist.RecentAlertsForEvent(ctx, eventSlug, since)
	if err != nil {
		return nil, err
	}
	return toConcentrationAlerts(rows), nil
}

func (a concentrationHistoryAdapter) RecentAlertsForWallet(ctx context.Context, wallet, eventSlug string, since time.Time) ([]concentration.Alert, error) {
	rows, err := a.hist.RecentAlertsForWallet(ctx, wallet, eventSlug, since)
	if err != nil {
		return nil, err
	}
	return toConcentrationAlerts(rows), nil
}

func toConcentrationAlerts(rows []repository.ConcentrationAlert) []concentration.Alert {
	out := make([]concentration.Alert, len(rows))
	for i, r := range rows {
		out[i] = concentration.Alert{
			CreatedAt:   r.CreatedAt,
			EventSlug:   r.EventSlug,
			Wallet:      r.Wallet,
			NotionalUSD: r.NotionalUSD,
			Severity:    r.Severity,
		}
	}
	return out
}

// Config wires the detector. Defaults fill in for zero-valued fields.
type Config struct {
	Thresholds     anomaly.Thresholds
	Baseline       baseline.Config
	Cluster        cluster.Config
	Filter         *category.Filter // nil => allow all (no filtering)
	PolymarketBase string           // "https://polymarket.com" (no trailing slash)
	GrafanaBase    string           // public base URL for Grafana deep-links; "" disables
	GrafanaDashUID string           // dashboard UID for deep-link
	GrafanaContext time.Duration    // ±window around trade time in Grafana link
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

	// LiveAlertMaxLag is the maximum acceptable distance between a
	// trade's traded_at and now() at the moment detect.Observe runs.
	// Trades older than this lag are NOT scored for live alerts —
	// the metric watchtower_trades_skipped_detection_total
	// {reason="too_old_for_live_alert"} increments and Observe
	// returns early. Baselines are NOT updated either (the trade
	// already lives in polymarket_trades and will be picked up by
	// the DB-backed baseline reader on the next legitimately-live
	// trade).
	//
	// This is the safety belt against an Observe call accidentally
	// firing on backfilled history. Default 0 disables the gate
	// (every trade is scored) — production should set this to a
	// duration matching the collect tick budget, e.g. 1h.
	LiveAlertMaxLag time.Duration

	Clock func() time.Time

	// Baseliner is the Postgres-backed baseline fetcher. Wired in
	// production whenever POSTGRES_DSN is set. Leave nil only in the
	// dev/local-only mode where the embedded baseline.Baseline reservoir
	// is read/written in-process (no persistence, no dedup, no
	// accumulation).
	Baseliner BaselineFetcher
	// TraderBaseliner serves the trader-history leg of the multiplier
	// ladder. Leave nil to disable the trader axis (v1 market-only
	// behaviour) — sensible default for local/debug paths and tests.
	TraderBaseliner TraderBaselineFetcher
	// MinTraderHistoryTrades is the count gate the trader baseline must
	// clear before its multiplier contributes to scoring. 0 disables the
	// trader axis regardless of TraderBaseliner availability.
	MinTraderHistoryTrades int
	// MMFilter suppresses single-trade alerts on wallets showing balanced
	// two-sided BUY+SELL activity on the same (market, outcome). nil =
	// disabled.
	MMFilter MMArbFilter

	// Accumulator is the same-trader accumulation-line detector. nil =
	// disabled. When both Accumulator and AccumulationLines are set the
	// detector evaluates an accumulation line per ingested trade after
	// the single-trade emit path.
	Accumulator *accumulation.Detector
	// AccumulationLines is the DB read path the detector uses to materialise
	// the wallet's recent same-side activity. Satisfied by
	// *repository.TradeRepository.
	AccumulationLines AccumulationLineFetcher

	// Ownership is the market-ownership concentration detector (Strategy
	// E). nil = disabled. The detector piggybacks on the accumulation
	// path — when an accumulation alert fires AND OwnershipShares is
	// wired, the share-flow approximation is evaluated, and a SEPARATE
	// KindOwnership Finding is emitted at the qualifying tier. Per-tier
	// dedup keeps it from spamming. By coupling to the accumulation
	// firing condition we avoid a standalone worker AND ensure
	// ownership only fires on wallets with a meaningful position
	// shape — micro-trades cannot trigger it.
	Ownership *ownership.Detector
	// OwnershipShares satisfies the (wallet, market, outcome) share-flow
	// read; in production it's *repository.TradeRepository.
	OwnershipShares OwnershipSharesFetcher

	// QuietMarket is the quiet-market wake-up detector. nil = disabled.
	// When set together with LastTradeFetcher and (for single-trade) the
	// market Baseliner, it stamps Finding.QuietMarket + appends
	// QUIET_MARKET_WAKEUP to Finding.Reasons on qualifying single-trade
	// and accumulation alerts.
	QuietMarket *quietmarket.Detector
	// LastTradeFetcher backs QuietMarket's idle-gap computation by returning
	// the most recent traded_at strictly before the wake-up event.
	LastTradeFetcher LastTradeBeforeFetcher
	// Alerts wires the Postgres dedup primitive. When set, every fired
	// Finding is INSERT … ON CONFLICT DO NOTHING into polymarket_alerts
	// before being handed to the realtime Emitter. Conflicts suppress the
	// emit entirely so log/webhook stay in sync with the DB queue.
	Alerts AlertCreator
	// Concentration is the v10.8 per-event + per-wallet concentration
	// gate. nil disables the gate entirely (legacy behaviour). When
	// set, the gate runs BEFORE TryCreatePending so a suppressed
	// alert leaves no DB row, no dedup key, no audit trail.
	Concentration ConcentrationGate
	// ConcentrationHistory is the seam to the recent-alerts window
	// used by the gate. nil also disables the gate.
	ConcentrationHistory ConcentrationHistory
	// Markets resolves condition_id → DB market id for the alerts row.
	Markets MarketResolver
	// Traders resolves wallet → DB trader id for the alerts row.
	Traders TraderResolver
	// StrategyVersion is stamped on every alert row and woven into the
	// dedup_key so a config retune cannot ressurect ignored alerts.
	// Defaults to "v2" (strategy upgrade adds trader-axis multiplier +
	// MM/arb suppression on top of v1).
	StrategyVersion string

	// NewWallet gates the wallet-age context booster. A wallet is "new"
	// when its FirstSeenAt is younger than NewWallet.MaxAge OR its
	// stored history is shorter than NewWallet.MaxHistoryTrades. New-
	// wallet status is attached to single-trade and accumulation
	// Findings as reason codes — never as a standalone alert. Disabled
	// when NewWallet.Enabled is false OR cfg.Traders is nil.
	NewWallet NewWalletConfig

	// DormantWallet gates the dormant-wallet revival booster. A
	// non-new wallet that has been idle for ≥ MinIdle and just placed
	// a trade ≥ MinNotionalUSD gets DORMANT_WALLET_REVIVAL stamped.
	// Disabled when Enabled=false or TraderActivity is nil.
	DormantWallet DormantWalletConfig

	// TraderActivity backs the dormant-wallet booster's idle-gap
	// computation. Production: *repository.TradeRepository.
	TraderActivity TraderActivityFetcher

	// --- v11.6 strategy learning loop hook ----------------------------
	// StrategyShadowBus is the strategybus.Bus the detector calls once
	// per newly-persisted alert. nil = disabled (default). The bus
	// owns the per-strategy enabled / shadow-only gate, so leaving
	// every strategy's flag off keeps the hook inert.
	StrategyShadowBus StrategyShadowSink
	// StrategyRulesRisk runs deterministic resolution-ambiguity scoring
	// on the alert's market. nil = disabled. The detector is pure.
	StrategyRulesRisk *rulesrisk.Detector
	// StrategyShadowMaxPerTrade caps how many shadow rows one trade
	// may spawn. Defaults to 20 if non-positive.
	StrategyShadowMaxPerTrade int
	// StrategyShadowRecordNoFire — when true, also write a shadow row
	// when a detector returned a "no fire" verdict. Defaults to false
	// to avoid row-volume blowup on every benign trade.
	StrategyShadowRecordNoFire bool
	// StrategyStagedInputs — v11.8/v11.12. Bridges background-worker
	// output (market_links, risk_scores, repricing_windows,
	// wallet_graph_edges, event_catalysts, recent shadow_decisions,
	// holder_snapshots, book_feature_bars) to the hot path. nil
	// makes the loop fall back to the v11.6 rulesrisk-only behaviour
	// (every other strategy is metric-skipped with reason
	// `staged_inputs_disabled`).
	//
	// The interface (not the concrete *stagedinputs.Readers) is
	// stored so tests can inject deterministic fakes without
	// standing up Postgres.
	StrategyStagedInputs StagedReaders

	// --- v11.12-insider-prior typed detector configs --------------
	// The hot path used to instantiate detectors with hardcoded
	// thresholds; v11.12 wires the parsed StrategyConfig in so
	// operator ENV (THESIS_ACCUM_*, OWNERSHIP_V2_*, etc.) is the
	// single source of truth. Zero-value structs fall through to
	// detector defaults (per-package applyDefaults).
	StrategyThesisAccum     ThesisAccumRuntimeConfig
	StrategyHolderDelta     HolderDeltaRuntimeConfig
	StrategyCatalystWindow  CatalystWindowRuntimeConfig
	StrategyBookVacuum      BookVacuumRuntimeConfig
	StrategyRepricingLag    RepricingLagRuntimeConfig
	StrategyWalletCohort    WalletCohortRuntimeConfig
	StrategyConflictResolve ConflictResolveRuntimeConfig
	StrategyCheapTail       CheapTailRuntimeConfig
	// StrategyMarketRegime is the stateless regime classifier. nil
	// disables the regime tag on shadow features (no regime row,
	// no metric); set to a constructed *marketregime.Classifier to
	// stamp every shadow row with classified regime + reasons.
	StrategyMarketRegime MarketRegimeClassifier
}

// ThesisAccumRuntimeConfig mirrors the operator-tunable subset of
// thesisaccum.Config that the hot path passes through. Lookbacks are
// kept in hours for the staged reader; the detector itself doesn't
// need lookback inside Decide().
type ThesisAccumRuntimeConfig struct {
	LookbackLifetime  time.Duration
	MinBreadth        int
	MinConsistency    float64
	MinAlignedScore   float64
	CatalystBoostMax  float64
	LiquidityFloorUSD float64
	MaxLinkedMarkets  int
}

// HolderDeltaRuntimeConfig — operator knobs for holderdelta.
type HolderDeltaRuntimeConfig struct {
	MinPctOIInfo        float64
	MinPctOIWarning     float64
	MinPctOICritical    float64
	TopK                int
	MinSharesDelta      float64
	OIShrinkPenalty     float64
	FreshSnapshotMaxAge time.Duration
}

// CatalystWindowRuntimeConfig — per-kind pre/post windows + the
// MinConfidence floor.
type CatalystWindowRuntimeConfig struct {
	MinConfidence         float64
	DebatePre             time.Duration
	DebatePost            time.Duration
	CourtRulingPre        time.Duration
	CourtRulingPost       time.Duration
	ElectionDayPre        time.Duration
	ElectionDayPost       time.Duration
	OfficialStatementPre  time.Duration
	OfficialStatementPost time.Duration
	GenericPre            time.Duration
	GenericPost           time.Duration
}

// BookVacuumRuntimeConfig — circuit-breaker thresholds + a bar
// freshness gate the hot path enforces before passing the detector.
type BookVacuumRuntimeConfig struct {
	MinCollapsePct float64
	MinSpreadZ     float64
	MinMidShiftPct float64
	MaxAgeBar      time.Duration
	TopN           int
}

// RepricingLagRuntimeConfig — lag floor / peer floor / ambiguity cap.
type RepricingLagRuntimeConfig struct {
	MinLagCents  float64
	PeerMinCount int
	MaxAmbiguity float64
}

// WalletCohortRuntimeConfig — co-trade + fresh-wallet burst knobs.
type WalletCohortRuntimeConfig struct {
	MinSimilarity       float64
	MinEvents           int
	MinCohortHits       int
	ConvergenceWindow   time.Duration
	FreshWalletMinBurst int
	FreshWalletMaxAge   time.Duration
}

// ConflictResolveRuntimeConfig — arbitration knobs.
type ConflictResolveRuntimeConfig struct {
	Window        time.Duration
	MinDominance  float64
	MMPenalty     float64
	MinQualitySum float64
}

// CheapTailRuntimeConfig — cheap-tail band + ambiguity cap. The
// load-bearing change in v11.12-insider-prior is the wider band
// (0.03..0.25) and the AmbiguityCutoff that wires rulesrisk.
type CheapTailRuntimeConfig struct {
	MinPrice        float64
	MaxPrice        float64
	MinNotionalUSD  float64
	MinTrades       int
	RequireCatalyst bool
	AmbiguityCutoff float64
}

// StagedReaders is the narrow surface detect.Loop needs from the
// stagedinputs bundle. *stagedinputs.Readers satisfies it implicitly.
// Defined here so tests can inject deterministic fakes without a
// live Postgres pool, and so the detect package doesn't grow new
// dependencies on every reader added in the analytics layer.
type StagedReaders interface {
	Enabled() bool
	CatalystsByEvent(ctx context.Context, eventSlug string) ([]stagedinputs.Catalyst, error)
	WalletEdgesForWallet(ctx context.Context, wallet string, edgeVersion int) ([]stagedinputs.WalletEdge, error)
	RecentDecisionsForCondition(ctx context.Context, conditionID string, since time.Time) ([]stagedinputs.RecentDecision, error)
	ClosedRepricingWindowsForCondition(ctx context.Context, conditionID string, since time.Time) ([]stagedinputs.RepricingWindow, error)
	MarketLinksByEvent(ctx context.Context, eventSlug string, linkVersion int) ([]stagedinputs.MarketLink, error)
	WalletThesisLinesForEvent(ctx context.Context, eventSlug, wallet string, lookbackHours int) ([]stagedinputs.WalletThesisLine, error)
	RiskScoreForCondition(ctx context.Context, conditionID string) (stagedinputs.RiskScore, bool, error)
	HolderSnapshotPairForWallet(ctx context.Context, conditionID, outcomeToken, wallet string) (stagedinputs.HolderSnapshotPair, bool, error)
	RecentBookFeatureBars(ctx context.Context, conditionID, outcomeToken string, since time.Time, rowLimit int) ([]stagedinputs.BookFeatureBar, error)
}

// MarketRegimeClassifier is the narrow surface detect.Loop needs from
// the marketregime classifier. Pure / stateless / no I/O.
type MarketRegimeClassifier interface {
	Classify(in MarketRegimeInput) MarketRegimeVerdict
}

// MarketRegimeInput / MarketRegimeVerdict are local mirrors of the
// public marketregime types so detect/ does not need to import the
// concrete package at the struct level (avoids any future import
// cycle when marketregime grows persistent state). The wiring layer
// (app.go) constructs the real classifier and adapts it through this
// interface.
type MarketRegimeInput struct {
	CategorySlug    string
	CategoryLabel   string
	Title           string
	Description     string
	ResolutionRules string
	EventSlug       string
}

type MarketRegimeVerdict struct {
	Regime  string
	Score   float64
	Reasons []string
}

// DormantWalletConfig tunes the dormant-wallet revival booster.
type DormantWalletConfig struct {
	Enabled        bool
	MinIdle        time.Duration
	MinNotionalUSD float64
}

// NewWalletConfig tunes the new-wallet context booster (Strategy B).
// The gate uses two complementary signals — recency of first activity
// (age from FirstSeenAt) and depth of history (trade count). A wallet
// is "new" when EITHER signal trips, so a long-dormant wallet that just
// reactivated with a single big bet still qualifies.
type NewWalletConfig struct {
	Enabled bool
	// MaxAge is the cutoff for "new" by FirstSeenAt. 168h (7d) is the
	// default. 0 disables the age leg (history-only).
	MaxAge time.Duration
	// MaxHistoryTrades is the cutoff for "new" by trade count. A wallet
	// with ≤ this many stored trades is flagged regardless of age. 10
	// is the default. 0 disables the history leg (age-only).
	MaxHistoryTrades int
}

// Loop owns the analytics state.
type Loop struct {
	cfg      Config
	cache    *marketcache.Cache
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
	cache *marketcache.Cache,
	emit Emitter,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
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
		cfg.StrategyVersion = anomaly.StrategyIdentity
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
		cache:    cache,
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
	// LIVE_ALERT_MAX_LAG safety belt. A trade reaching detect.Observe
	// with a traded_at older than the configured lag is almost
	// certainly being replayed from polymarket_trades — either
	// backfill leaked into the Observe path (architectural bug) or
	// the operator restarted the binary after long downtime and the
	// collector's bootstrap window swept up stale history. Either
	// way we must not send a Telegram alert: the public market has
	// long since priced this trade in.
	//
	// We still record the trade size on the histogram so dashboards
	// stay consistent; baseline updates happen via persist.Sink on
	// the way in, not here. Nil-guarded — Loops constructed without
	// metrics (some test paths, CLI subcommands) must not crash on
	// the hot path.
	if l.metrics != nil && l.metrics.TradeSizeUSD != nil {
		l.metrics.TradeSizeUSD.Observe(notional)
	}
	if l.cfg.LiveAlertMaxLag > 0 && !trade.Timestamp.IsZero() {
		if l.now().Sub(trade.Timestamp) > l.cfg.LiveAlertMaxLag {
			if l.metrics != nil && l.metrics.TradesSkippedDetection != nil {
				l.metrics.TradesSkippedDetection.WithLabelValues("too_old_for_live_alert").Inc()
			}
			return
		}
	}
	if l.metrics != nil && l.metrics.TradesAnalyzed != nil {
		l.metrics.TradesAnalyzed.Inc()
	}

	// Alert-eligibility gates. These do NOT block baseline updates — we want
	// the reservoir to warm continuously so it's ready the moment the market
	// crosses the lifecycle threshold.
	lifecyclePct, lifecycleKnown := market.LifecyclePct(trade.Timestamp)
	hot := lifecycleKnown && lifecyclePct >= l.cfg.LifecycleHotFromPct
	gateAllowsAlert := true
	if !lifecycleKnown {
		// Fail-closed by design — a market without start/end dates cannot
		// be lifecycle-gated, and the lifecycle gate is the single most
		// load-bearing precision floor of the entire pipeline. There is
		// no env override; bumping baseline volume on the upstream
		// `MarketsTracked` metric tells the operator how many markets
		// fall into this bucket.
		gateAllowsAlert = false
		if l.metrics != nil && l.metrics.LifecycleUnknownSkipped != nil {
			l.metrics.LifecycleUnknownSkipped.Inc()
		}
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

	// Trader-history baseline is independent of category, so fetch it once per
	// trade (not per-category). When the trader axis is disabled or the wallet
	// is unknown the result is an empty baseline.Stats; the scorer treats that
	// as "this leg does not contribute" and falls back to the market-only
	// behaviour of v1.
	traderStats := l.readTraderBaseline(ctx, trade.Taker)

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

		// Market-baseline readiness gate. v2: when the market baseline is
		// unready (cold market, thin span) we mask the MARKET axis only,
		// leaving the trader axis free to contribute. Passing empty stats to
		// the scorer disables the market leg cleanly.
		marketStats := stats
		if l.cfg.BaselineMinReadySpan > 0 && stats.SpanActual < l.cfg.BaselineMinReadySpan {
			marketStats = baseline.Stats{}
		}

		// Trader-axis readiness gate: count floor enforced HERE (the scorer
		// itself only checks median > 0 and the shared count/total floors).
		traderInput := traderStats
		if l.cfg.MinTraderHistoryTrades > 0 && traderStats.Count < l.cfg.MinTraderHistoryTrades {
			traderInput = baseline.Stats{}
		}

		sr := score.Score(notional, trade.Price, marketStats, traderInput, l.cfg.Thresholds)
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
		l.emitTradeAnomaly(ctx, market, trade, bestCat, bestStats, traderStats, bestResult, bestRef, lifecyclePct, hot)
	}

	// Same-trader accumulation line. Runs after the single-trade decision
	// and is intentionally independent of it — a wallet building exposure
	// in small slices can fire accumulation even when no individual trade
	// reached the single-trade threshold. The line query is per-(wallet,
	// market, outcome, side) and backed by a composite index.
	//
	// Lifecycle / category whitelist are applied; trader stats are reused.
	// MM filter is also reused — balanced two-sided activity is the same
	// false-positive shape here as it is for single-trade.
	if gateAllowsAlert {
		// Evaluate BOTH accumulation horizons. Recent catches bursty
		// accumulation inside the Cooldown window; lifetime catches
		// slow-drip conviction across days/weeks/months. Per-window
		// dedup keys keep them from spamming each other.
		l.evaluateAccumulationWindow(ctx, market, trade, categories, traderStats, lifecyclePct, hot, accumulation.WindowKindRecent)
		l.evaluateAccumulationWindow(ctx, market, trade, categories, traderStats, lifecyclePct, hot, accumulation.WindowKindLifetime)
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

// readTraderBaseline returns the wallet's full-history distribution. When
// cfg.TraderBaseliner is nil (local/debug, or wallet unknown) the trader
// axis is disabled and v1 market-only scoring applies. Errors are logged
// and treated as "axis disabled" so a DB hiccup never blocks emission of
// a market-side alert.
func (l *Loop) readTraderBaseline(ctx context.Context, wallet string) baseline.Stats {
	if l.cfg.TraderBaseliner == nil || wallet == "" {
		return baseline.Stats{}
	}
	stats, err := l.cfg.TraderBaseliner.Stats(ctx, wallet)
	if err != nil {
		l.log.Err(err).Str("wallet", wallet).Msg("detect: trader baseline read failed; trader axis disabled for this trade")
		return baseline.Stats{}
	}
	return stats
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
	traderStats baseline.Stats,
	sr score.Result,
	ref anomaly.TradeRef,
	lifecyclePct float64,
	hot bool,
) {
	catRef := l.categoryRef(cat)
	scope := fmt.Sprintf("category=%s market=%s outcome=%s",
		nonEmpty(catRef.Label, "uncategorised"), m.Slug, nonEmpty(ref.Outcome, "?"))
	peerCount := l.cluster.Count(cat)

	// MM/arbitrage suppression: a candidate that fires single-trade is
	// blocked from emission when the wallet's two-sided activity on the
	// same (market, outcome) looks like liquidity provision / arbitrage.
	// Cluster alerts are unaffected (they have their own gates).
	if l.cfg.MMFilter != nil && ref.Wallet != "" {
		mid := l.resolveMarketID(ctx, m.ID)
		var midVal int64
		if mid != nil {
			midVal = *mid
		}
		v, err := l.cfg.MMFilter.Decide(ctx, ref.Wallet, midVal, string(t.Token))
		if err != nil {
			l.log.Err(err).Str("wallet", ref.Wallet).Msg("detect: mm filter error; failing open")
		}
		if v.Suppress {
			l.metrics.AlertMMSuppressed.WithLabelValues(categoryLabel(catRef), mmfilter.ReasonPossibleMarketMaker).Inc()
			l.log.Info().
				Str("reason_code", mmfilter.ReasonPossibleMarketMaker).
				Str("kind", string(anomaly.KindTradeAnomaly)).
				Str("wallet", ref.Wallet).
				Str("market", string(m.ID)).
				Str("outcome", string(t.Token)).
				Str("detail", v.Reason).
				Int64("buy_count", v.BuyCount).
				Int64("sell_count", v.SellCount).
				Float64("buy_notional_usd", v.BuyNotionalUSD).
				Float64("sell_notional_usd", v.SellNotionalUSD).
				Float64("imbalance", v.Imbalance).
				Msg("detect: single-trade alert suppressed (mm/arb signature)")
			return
		}
	}

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
			P99USD:    stats.P99USD,
			SampleN:   stats.Count,
			Span:      stats.SpanActual,
			WindowMax: l.cfg.Baseline.Window,
		},
		GrossPayoutIfWinUSD:         sr.GrossPayoutIfWinUSD,
		ProfitIfWinUSD:              sr.ProfitIfWinUSD,
		MarketP95Ratio:              sr.MarketP95Ratio,
		MarketP99Ratio:              sr.MarketP99Ratio,
		TraderP95Ratio:              sr.TraderP95Ratio,
		TraderP99Ratio:              sr.TraderP99Ratio,
		PayoffGatePassed:            sr.PayoffGatePassed,
		TailGatePassed:              sr.TailGatePassed,
		LowMarketBaselineConfidence: sr.LowMarketBaselineConfidence,
		LowTraderBaselineConfidence: sr.LowTraderBaselineConfidence,
		SeverityCapped:              sr.SeverityCapped,
		LifecyclePct:                lifecyclePct,
		Hot:                         hot,
		InCluster:                   peerCount >= 2,
		ClusterPeerCount:            peerCount,
		MarketURL:                   l.marketURL(m),
		CategoryURL:                 l.categoryURL(catRef),
		TraderURL:                   l.traderURL(ref.Wallet),
		GrafanaURL:                  l.grafanaURL(catRef, m, t.Timestamp, sr.Severity),
	}
	// Trader-history context (when available) — operators want to see "this
	// wallet's typical trade is $X" alongside the market-side baseline.
	if traderStats.Count > 0 {
		f.TraderBaseline = &anomaly.BaselineRef{
			Scope:     "trader=" + ref.Wallet,
			MedianUSD: traderStats.MedianUSD,
			MeanUSD:   traderStats.MeanUSD,
			P95USD:    traderStats.P95USD,
			P99USD:    traderStats.P99USD,
			SampleN:   traderStats.Count,
			Span:      traderStats.SpanActual,
		}
	}
	// Quiet-market wake-up: tag the alert when the market/outcome was
	// historically quiet AND the current trade is large enough to wake it.
	// Context-only — does not change the firing decision (already made
	// above by score.Score).
	l.stampQuietMarket(ctx, &f, m, baseline.Key{Category: cat, Market: m.ID, OutcomeToken: t.Token},
		stats, ref.NotionalUSD, t.Timestamp)
	// New-wallet context booster: attaches NEW_WALLET_LARGE_BET (and
	// LOW_TRADER_HISTORY when the wallet's stored history is thin) when
	// the firing wallet matches Config.NewWallet thresholds.
	if nw := l.newWalletRef(ctx, ref.Wallet, traderStats.Count); nw != nil {
		f.NewWallet = nw
		f.Reasons = append(f.Reasons, anomaly.ReasonNewWalletLargeBet)
		if l.cfg.NewWallet.MaxHistoryTrades > 0 && traderStats.Count <= l.cfg.NewWallet.MaxHistoryTrades {
			f.Reasons = append(f.Reasons, anomaly.ReasonLowTraderHistory)
		}
		if l.metrics != nil && l.metrics.NewWalletReasons != nil {
			l.metrics.NewWalletReasons.WithLabelValues(string(anomaly.KindTradeAnomaly), string(sr.Severity)).Inc()
		}
	}

	// Dormant-wallet context booster: non-new wallet that has been
	// idle for ≥ DORMANT_WALLET_MIN_IDLE and just placed a sized
	// trade. Context only — never escalates severity.
	if dw := l.dormantWalletRef(ctx, ref.Wallet, ref.NotionalUSD, t.Timestamp, f.NewWallet); dw != nil {
		f.DormantWallet = dw
		f.Reasons = append(f.Reasons, anomaly.ReasonDormantWalletRevival)
	}

	// Ownership fusion: when the same (wallet, market, outcome) has
	// trade-flow concentration, stamp the ownership context on this
	// single-trade Finding so an operator sees "this trade ALSO sits
	// on top of a 12%-of-flow position". Never promotes severity.
	if or := l.ownershipFusionRef(ctx, ref.Wallet, m.ID, t.Token, t.Price); or != nil {
		f.Ownership = or
		f.Reasons = append(f.Reasons, anomaly.ReasonOwnershipFusion, anomaly.ReasonMarketOwnershipConcentration)
		if or.Approximate {
			f.Reasons = append(f.Reasons, "APPROXIMATE_OWNERSHIP")
		}
	}

	// Surface v6 low-baseline reasons on the alert payload so the
	// operator sees the uncertainty even if Telegram rendering hasn't
	// surfaced the dedicated row yet.
	if sr.LowMarketBaselineConfidence {
		f.Reasons = append(f.Reasons, anomaly.ReasonLowMarketBaselineConfidence)
	}
	if sr.LowTraderBaselineConfidence {
		f.Reasons = append(f.Reasons, anomaly.ReasonLowTraderBaselineConfidence)
	}
	if sr.SeverityCapped {
		f.Reasons = append(f.Reasons, anomaly.ReasonSeverityCappedLowBaseline)
	}

	l.metrics.TradeAnomalies.WithLabelValues(string(sr.Severity), categoryLabel(catRef), anomaly.ReasonSingle).Inc()
	if sr.MarketP95Ratio > 0 {
		l.metrics.TradeMarketP95Ratio.Observe(sr.MarketP95Ratio)
	}
	if sr.TraderP95Ratio > 0 {
		l.metrics.TradeTraderP95Ratio.Observe(sr.TraderP95Ratio)
	}
	if sr.ProfitIfWinUSD > 0 {
		l.metrics.TradeProfitIfWinUSD.Observe(sr.ProfitIfWinUSD)
	}
	if ref.Odds > 0 {
		l.metrics.TradeOdds.Observe(ref.Odds)
	}
	l.metrics.CategoryAnomalousTrades.WithLabelValues(categoryLabel(catRef), string(sr.Severity)).Inc()
	l.metrics.CategoryAnomalousUSD.WithLabelValues(categoryLabel(catRef), string(sr.Severity)).Add(ref.NotionalUSD)

	dedup := l.singleTradeDedupKey(m, t)
	f.DedupKey = dedup
	if !l.persistAlert(ctx, repository.AlertKindTrade, dedup, m, t, f) {
		// DB dedup said "already alerted" — keep realtime sinks in sync.
		return
	}
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit single-trade failed")
	}
	// v11.6: shadow-write per-alert strategy decisions. Fails open;
	// never blocks the existing alert flow.
	l.recordStrategyShadow(ctx, m, t, f, dedup)
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
	f.DedupKey = dedup
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
	// v10.8 concentration gate. Runs BEFORE TryCreatePending so a
	// suppressed alert leaves no DB row, no dedup key, no audit
	// trail. The gate is fail-open: any error is logged and the
	// alert proceeds.
	if l.cfg.Concentration != nil && l.cfg.ConcentrationHistory != nil {
		notional := t.NotionalUSD()
		dec, err := l.cfg.Concentration.Evaluate(ctx,
			concentration.Candidate{
				EventSlug:   m.EventSlug,
				Wallet:      t.Taker,
				NotionalUSD: notional,
				Severity:    string(f.Severity),
				Now:         t.Timestamp,
			},
			concentrationHistoryAdapter{hist: l.cfg.ConcentrationHistory},
		)
		if err == nil && !dec.Allow {
			l.log.Info().
				Str("event_slug", m.EventSlug).
				Str("wallet", t.Taker).
				Str("severity", string(f.Severity)).
				Float64("notional_usd", notional).
				Float64("required", dec.RequiredNotional).
				Int("priors", dec.PriorAlertsInWindow).
				Float64("max_prev", dec.MaxPriorNotional).
				Str("reason", dec.Reason).
				Msg("detect: concentration gate suppressed alert")
			l.observeConcentrationSuppressed(dec.Reason)
			return false
		}
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

// observeConcentrationSuppressed bumps the v10.8 suppression metric.
// Reason label values: wallet_escalation_failed | event_concentration_cap.
func (l *Loop) observeConcentrationSuppressed(reason string) {
	if l.metrics == nil || l.metrics.ConcentrationSuppressed == nil {
		return
	}
	l.metrics.ConcentrationSuppressed.WithLabelValues(reason).Inc()
}

// evaluateAccumulationWindow runs the same-trader accumulation detector
// for the trade's (wallet, market, outcome, side) bucket against a
// specific horizon. The horizon controls only the SQL Since cutoff and
// the dedup-key namespace; the detector math is identical for both.
//
// Skips quietly when:
//   - the accumulator is disabled or unwired (cfg.Accumulator / cfg.AccumulationLines nil);
//   - the wallet has not been persisted yet (no trader_id);
//   - no category in the trade's set passes the whitelist;
//   - the line summary returned no trades or fewer than the detector floor;
//   - the MM/arb filter classifies the wallet two-sided.
//
// Severity emission, metrics, dedup, and realtime sink fanout follow the
// same shape as the single-trade path so the two signals are
// operationally uniform.
func (l *Loop) evaluateAccumulationWindow(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	categories []vo.CategoryID,
	traderStats baseline.Stats,
	lifecyclePct float64,
	hot bool,
	wk accumulation.WindowKind,
) {
	if l.cfg.Accumulator == nil || l.cfg.AccumulationLines == nil {
		return
	}
	if t.Taker == "" || t.Token == "" {
		return
	}
	// Need the persisted trader id to even query the line — accumulation
	// is intrinsically wallet-scoped.
	tid := l.resolveTraderID(ctx, t.Taker)
	if tid == nil {
		return
	}
	mid := l.resolveMarketID(ctx, m.ID)
	if mid == nil {
		return
	}

	// Pick the first whitelisted category in the trade's set as the
	// reporting category. Accumulation isn't per-category in the model,
	// but the Telegram payload and metrics want one.
	var cat vo.CategoryID
	for _, c := range categories {
		if l.allowed(c) {
			cat = c
			break
		}
	}
	if cat == 0 && len(categories) > 0 {
		// Whitelist didn't match any category for this market — accumulation
		// stays silent. (Defense-in-depth: discover should have stripped
		// non-whitelisted ids before they ever got here.)
		return
	}

	// Recent windows are bounded by the configured Cooldown-window; the
	// lifetime window passes a zero time so the SQL's NULL-since branch
	// reads every stored trade for this (wallet, market, outcome, side).
	var since time.Time
	if wk == accumulation.WindowKindRecent {
		since = l.now().Add(-l.cfg.Accumulator.Config().Window)
	}
	rep, err := l.cfg.AccumulationLines.AccumulationLineSummary(ctx, repository.AccumulationLineQuery{
		TraderID:     *tid,
		MarketID:     *mid,
		OutcomeToken: string(t.Token),
		Side:         string(t.Side),
		Since:        since,
	})
	if err != nil {
		l.log.Err(err).Str("wallet", t.Taker).Msg("detect: accumulation line read failed")
		return
	}
	if rep.TradeCount == 0 {
		return
	}

	// Project repository roll-up into the detector's Line shape.
	line := accumulation.Line{
		Wallet:            t.Taker,
		MarketID:          string(m.ID),
		OutcomeToken:      string(t.Token),
		Side:              t.Side,
		Window:            wk,
		TradeCount:        int(rep.TradeCount),
		TotalNotionalUSD:  rep.TotalNotionalUSD,
		MeanNotionalUSD:   rep.MeanNotionalUSD,
		MedianNotionalUSD: rep.MedianNotionalUSD,
		MaxNotionalUSD:    rep.MaxNotionalUSD,
		MinNotionalUSD:    rep.MinNotionalUSD,
		AvgOdds:           rep.AvgOdds(),
		MaxOdds:           rep.MaxOdds(),
		OldestAt:          rep.OldestAt,
		NewestAt:          rep.NewestAt,
		MarketMedianUSD:   0,
		MarketP95USD:      0,
		TraderMedianUSD:   traderStats.MedianUSD,
		TraderP95USD:      traderStats.P95USD,
		LifecyclePct:      lifecyclePct,
		Hot:               hot,
	}
	// Market baseline for the multiplier check. Use the production
	// Baseliner when wired (Postgres). In dev/test mode fall back to the
	// in-process reservoir so accumulation still works without a DB.
	bk := baseline.Key{Category: cat, Market: m.ID, OutcomeToken: t.Token}
	var marketStats baseline.Stats
	if l.cfg.Baseliner != nil {
		var mErr error
		marketStats, mErr = l.cfg.Baseliner.Stats(ctx, bk)
		if mErr != nil {
			l.log.Err(mErr).Msg("detect: baseline read for accumulation failed")
		}
	} else {
		marketStats = l.baseline.Stats(bk)
	}
	line.MarketMedianUSD = marketStats.MedianUSD
	line.MarketP95USD = marketStats.P95USD

	v := l.cfg.Accumulator.Decide(line)
	if !v.Fired {
		return
	}

	// MM/arb suppression — balanced two-sided activity is the same FP shape
	// here as for single-trade.
	if l.cfg.MMFilter != nil {
		mmv, mmErr := l.cfg.MMFilter.Decide(ctx, t.Taker, *mid, string(t.Token))
		if mmErr != nil {
			l.log.Err(mmErr).Msg("detect: mm filter (accumulation) error; failing open")
		}
		if mmv.Suppress {
			l.metrics.AlertMMSuppressed.WithLabelValues(categoryLabelByID(l, cat), mmfilter.ReasonPossibleMarketMaker).Inc()
			l.log.Info().
				Str("reason_code", mmfilter.ReasonPossibleMarketMaker).
				Str("kind", string(anomaly.KindAccumulation)).
				Str("wallet", t.Taker).
				Str("market", string(m.ID)).
				Str("outcome", string(t.Token)).
				Str("detail", mmv.Reason).
				Int64("buy_count", mmv.BuyCount).
				Int64("sell_count", mmv.SellCount).
				Float64("buy_notional_usd", mmv.BuyNotionalUSD).
				Float64("sell_notional_usd", mmv.SellNotionalUSD).
				Float64("imbalance", mmv.Imbalance).
				Msg("detect: accumulation alert suppressed (mm/arb signature)")
			return
		}
	}

	catRef := l.categoryRef(cat)
	reasonStrs := make([]string, 0, len(v.Reasons))
	for _, r := range v.Reasons {
		reasonStrs = append(reasonStrs, string(r))
	}
	f := anomaly.Finding{
		Kind:     anomaly.KindAccumulation,
		Severity: v.Severity,
		At:       l.now(),
		Reason:   anomaly.ReasonAccumulation,
		Trade: &anomaly.TradeRef{
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
			Odds:        line.MaxOdds,
			NotionalUSD: rep.TotalNotionalUSD, // line total for sink convenience
			At:          t.Timestamp,
		},
		Category: &catRef,
		Accumulation: &anomaly.AccumulationRef{
			Wallet:            t.Taker,
			MarketID:          string(m.ID),
			OutcomeToken:      string(t.Token),
			Outcome:           m.OutcomeLabel(t.Token),
			Side:              string(t.Side),
			TradeCount:        int(rep.TradeCount),
			TotalNotionalUSD:  rep.TotalNotionalUSD,
			MeanNotionalUSD:   rep.MeanNotionalUSD,
			MedianNotionalUSD: rep.MedianNotionalUSD,
			MaxNotionalUSD:    rep.MaxNotionalUSD,
			AvgOdds:           line.AvgOdds,
			MaxOdds:           line.MaxOdds,
			Span:              rep.Span(),
			MarketMultiplier:  v.LineMarketMultiplier,
			TraderMultiplier:  v.LineTraderMultiplier,
			Score:             v.Score,
			Confidence:        v.Confidence,
			Reasons:           reasonStrs,
			SizePath:          v.SizePath,
			Window:            string(wk),
		},
		Baseline: &anomaly.BaselineRef{
			Scope:     fmt.Sprintf("category=%s market=%s outcome=%s", nonEmpty(catRef.Label, "uncategorised"), m.Slug, nonEmpty(m.OutcomeLabel(t.Token), "?")),
			MedianUSD: line.MarketMedianUSD,
			P95USD:    line.MarketP95USD,
			Span:      0, // not directly relevant on a line-level alert
		},
		LifecyclePct: lifecyclePct,
		Hot:          hot,
		MarketURL:    l.marketURL(m),
		CategoryURL:  l.categoryURL(catRef),
		TraderURL:    l.traderURL(t.Taker),
		GrafanaURL:   l.grafanaURL(catRef, m, t.Timestamp, v.Severity),
	}
	if traderStats.Count > 0 {
		f.TraderBaseline = &anomaly.BaselineRef{
			Scope:     "trader=" + t.Taker,
			MedianUSD: traderStats.MedianUSD,
			MeanUSD:   traderStats.MeanUSD,
			P95USD:    traderStats.P95USD,
			SampleN:   traderStats.Count,
			Span:      traderStats.SpanActual,
		}
	}
	// Quiet-market wake-up: an accumulation line on a quiet market is the
	// canonical informed-flow shape. The line's oldest trade is the right
	// anchor for "when did this wake-up begin" — earlier events make the
	// idle gap larger and the signal stronger. bk was already declared
	// upstream when reading the market baseline; reuse it here.
	marketHist := l.marketHistoryStats(ctx, bk)
	if marketHist.Count == 0 {
		// Fall back to the line itself if the market baseline is empty —
		// gives the detector something to judge against (sample-count=1 is
		// still below the quiet-market readiness floor unless the operator
		// has dropped MaxTradesPerDay extremely low).
		marketHist = baseline.Stats{
			Count:      int(rep.TradeCount),
			MedianUSD:  line.MarketMedianUSD,
			SpanActual: rep.Span(),
		}
	}
	l.stampQuietMarket(ctx, &f, m, bk, marketHist, rep.TotalNotionalUSD, rep.OldestAt)
	// Append a window-tagging reason so an operator reading the alert in
	// Telegram can tell a 24h burst (RECENT_ACCUMULATION) from a slow
	// drip (LIFETIME_ACCUMULATION). The accumulation Finding renderer
	// flattens these into the Why block alongside the detector's
	// per-tier reasons.
	switch wk {
	case accumulation.WindowKindLifetime:
		f.Reasons = append(f.Reasons, anomaly.ReasonLifetimeAccumulation)
	case accumulation.WindowKindRecent:
		f.Reasons = append(f.Reasons, anomaly.ReasonRecentAccumulation)
	}
	// New-wallet context booster: a fresh wallet that has already built
	// a meaningful line is qualitatively more suspicious than the same
	// line from a long-history wallet.
	if nw := l.newWalletRef(ctx, t.Taker, traderStats.Count); nw != nil {
		f.NewWallet = nw
		f.Reasons = append(f.Reasons, anomaly.ReasonNewWalletAccumulation)
		if l.cfg.NewWallet.MaxHistoryTrades > 0 && traderStats.Count <= l.cfg.NewWallet.MaxHistoryTrades {
			f.Reasons = append(f.Reasons, anomaly.ReasonLowTraderHistory)
		}
		if l.metrics != nil && l.metrics.NewWalletReasons != nil {
			l.metrics.NewWalletReasons.WithLabelValues(string(anomaly.KindAccumulation), string(v.Severity)).Inc()
		}
	}
	l.metrics.AccumulationAlerts.WithLabelValues(string(v.Severity), categoryLabelByID(l, cat), string(wk)).Inc()

	dedup := l.accumulationDedupKey(wk, t.Taker, *mid, string(t.Token), string(t.Side), v.Severity)
	f.DedupKey = dedup
	if !l.persistAlert(ctx, repository.AlertKindAccumulation, dedup, m, t, f) {
		return
	}
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit accumulation failed")
	}
	// v11.6: shadow-write per-alert strategy decisions.
	l.recordStrategyShadow(ctx, m, t, f, dedup)

	// Strategy E hook: once an accumulation alert is established, check
	// whether the wallet's net position has crossed an ownership-
	// concentration tier. Coupling ownership to the accumulation firing
	// condition is the harmonization mechanic — ownership cannot fire
	// on micro-trades and never spams on its own. Only invoked from the
	// recent-window emission so a long-history line doesn't double-
	// emit ownership across both windows.
	if wk == accumulation.WindowKindRecent {
		l.evaluateOwnership(ctx, m, t, *tid, *mid, cat, t.Price)
	}
}

// evaluateOwnership runs the Strategy-E concentration check for the
// (wallet, market, outcome) the accumulation path just alerted on.
// Emits a separate KindOwnership Finding when the wallet's trade-flow-
// approximated share crossed at least the Info tier AND the absolute-
// notional floor. Per-tier dedup prevents repeated alerts at the same
// level; an upgrade (Info → Warning → Critical) emits one new alert at
// each tier.
//
// Returns silently when:
//   - the detector or its repository read is unwired;
//   - the trade-flow query returns zero denominator (impossible on a
//     real accumulating wallet but defensive);
//   - the verdict didn't fire.
func (l *Loop) evaluateOwnership(
	ctx context.Context,
	m market.Market,
	t trade.Trade,
	traderID, marketID int64,
	cat vo.CategoryID,
	priceHint float64,
) {
	if l.cfg.Ownership == nil || l.cfg.OwnershipShares == nil {
		return
	}
	row, err := l.cfg.OwnershipShares.OwnershipShares(ctx, repository.OwnershipSharesQuery{
		TraderID:     traderID,
		MarketID:     marketID,
		OutcomeToken: string(t.Token),
	})
	if err != nil {
		l.log.Err(err).Str("wallet", t.Taker).Msg("detect: ownership read failed")
		return
	}
	v := l.cfg.Ownership.Decide(ownership.Shares{
		WalletBuyShares:  row.WalletBuyShares,
		WalletSellShares: row.WalletSellShares,
		MarketBuyShares:  row.MarketBuyShares,
		PriceHint:        priceHint,
	})
	if !v.Fired {
		return
	}
	reasonStrs := make([]string, 0, len(v.Reasons))
	for _, r := range v.Reasons {
		reasonStrs = append(reasonStrs, string(r))
	}
	catRef := l.categoryRef(cat)
	f := anomaly.Finding{
		Kind:     anomaly.KindOwnership,
		Severity: v.Severity,
		At:       l.now(),
		Reason:   anomaly.ReasonOwnership,
		Trade: &anomaly.TradeRef{
			ID:          t.ID,
			Wallet:      t.Taker,
			Market:      m.ID,
			Slug:        m.Slug,
			Question:    m.Question,
			Outcome:     m.OutcomeLabel(t.Token),
			Side:        t.Side,
			SizeShares:  t.Size,
			Price:       t.Price,
			NotionalUSD: v.NotionalUSD, // wallet's position dollar estimate
			At:          t.Timestamp,
		},
		Category: &catRef,
		Ownership: &anomaly.OwnershipRef{
			Wallet:            t.Taker,
			MarketID:          string(m.ID),
			OutcomeToken:      string(t.Token),
			Outcome:           m.OutcomeLabel(t.Token),
			SharePct:          v.SharePct,
			WalletNetShares:   v.NetShares,
			MarketTotalShares: row.MarketBuyShares,
			NotionalUSD:       v.NotionalUSD,
			Approximate:       true,
		},
		Reasons:     reasonStrs,
		MarketURL:   l.marketURL(m),
		CategoryURL: l.categoryURL(catRef),
		TraderURL:   l.traderURL(t.Taker),
		GrafanaURL:  l.grafanaURL(catRef, m, t.Timestamp, v.Severity),
	}
	dedup := fmt.Sprintf("ownership:%s:%s:%d:%s:%s",
		l.cfg.StrategyVersion, t.Taker, marketID, string(t.Token), v.Severity)
	f.DedupKey = dedup
	if !l.persistAlert(ctx, repository.AlertKindOwnership, dedup, m, t, f) {
		return
	}
	if l.metrics != nil && l.metrics.OwnershipAlerts != nil {
		l.metrics.OwnershipAlerts.WithLabelValues(string(v.Severity), categoryLabelByID(l, cat)).Inc()
	}
	if err := l.emit.Notify(ctx, f); err != nil {
		l.log.Err(err).Msg("detect: emit ownership failed")
	}
	// v11.6: shadow-write per-alert strategy decisions.
	l.recordStrategyShadow(ctx, m, t, f, dedup)
}

// stampQuietMarket runs the quiet-market wake-up detector against the
// (market, outcome) and stamps Finding.QuietMarket + appends the canonical
// reason code to Finding.Reasons when the verdict qualifies. No-op when
// the detector is disabled or the supporting fetchers aren't wired.
//
// The function is intentionally tolerant: a missing LastTradeFetcher or a
// transient DB error degrades to "no prior trade" (zero LastTradedAt),
// which is the strongest possible quiet signal — passes the idle gate by
// default per the detector contract.
func (l *Loop) stampQuietMarket(
	ctx context.Context,
	f *anomaly.Finding,
	m market.Market,
	bk baseline.Key,
	hist baseline.Stats,
	currentNotionalUSD float64,
	currentAt time.Time,
) {
	if l.cfg.QuietMarket == nil {
		return
	}
	var lastBefore time.Time
	if l.cfg.LastTradeFetcher != nil {
		mid := l.resolveMarketID(ctx, m.ID)
		if mid != nil {
			t, err := l.cfg.LastTradeFetcher.LastTradedAtBefore(ctx, *mid, string(bk.OutcomeToken), currentAt)
			if err != nil {
				l.log.Err(err).Msg("detect: quiet-market last-trade lookup failed; degrading to no-prior")
			} else {
				lastBefore = t
			}
		}
	}
	v := l.cfg.QuietMarket.Decide(
		quietmarket.History{
			SampleCount:      int64(hist.Count),
			TotalNotionalUSD: hist.TotalUSD,
			Span:             hist.SpanActual,
			MarketMedianUSD:  hist.MedianUSD,
			LastTradedAt:     lastBefore,
		},
		quietmarket.Event{NotionalUSD: currentNotionalUSD, At: currentAt},
	)
	if !v.Qualifies {
		return
	}
	f.QuietMarket = &anomaly.QuietMarketRef{
		TradesPerDay:      v.TradesPerDay,
		NotionalPerDayUSD: v.NotionalPerDayUSD,
		IdleDuration:      v.IdleDuration,
		BaselineSpan:      v.BaselineSpan,
	}
	f.Reasons = append(f.Reasons, v.Reason)
	if l.metrics != nil && l.metrics.QuietMarketAlerts != nil {
		l.metrics.QuietMarketAlerts.WithLabelValues(string(f.Severity), string(f.Kind)).Inc()
	}
}

// marketHistoryStats returns the per-bucket baseline stats used by the
// quiet-market detector. Prefers the production Baseliner; falls back to
// the in-process reservoir for dev/test.
func (l *Loop) marketHistoryStats(ctx context.Context, bk baseline.Key) baseline.Stats {
	if l.cfg.Baseliner != nil {
		s, err := l.cfg.Baseliner.Stats(ctx, bk)
		if err != nil {
			l.log.Err(err).Msg("detect: quiet-market baseline read failed")
			return baseline.Stats{}
		}
		return s
	}
	return l.baseline.Stats(bk)
}

// accumulationDedupKey produces a window- and severity-aware dedup key.
//
//   - WindowKindRecent → "accumulation:<sv>:recent:<wallet>:<mid>:<token>:<side>:<bucket>"
//     bucket is floor(now / cooldown). Two firings on the same line
//     inside one cooldown window collide; the next bucket gets a fresh
//     key, matching the cooldown contract.
//
//   - WindowKindLifetime → "accumulation:<sv>:lifetime:<wallet>:<mid>:<token>:<side>:<severity>"
//     no bucket — exactly one alert per line per severity tier ever. A
//     line that crosses Info → Warning → Critical emits THREE distinct
//     alerts, one at each tier; a line that stays at Info emits exactly
//     one Info alert across its entire lifetime.
func (l *Loop) accumulationDedupKey(wk accumulation.WindowKind, wallet string, marketID int64, outcomeToken, side string, severity anomaly.Severity) string {
	switch wk {
	case accumulation.WindowKindLifetime:
		return fmt.Sprintf("accumulation:%s:lifetime:%s:%d:%s:%s:%s",
			l.cfg.StrategyVersion, wallet, marketID, outcomeToken, side, severity)
	default:
		// Recent (and any unknown kind) keeps the time-bucket dedup
		// behaviour that the cooldown contract expects.
		bucket := l.cfg.Accumulator.Config().Cooldown
		if bucket <= 0 {
			bucket = 30 * time.Minute
		}
		bucketStart := l.now().Truncate(bucket).Unix()
		return fmt.Sprintf("accumulation:%s:recent:%s:%d:%s:%s:%d",
			l.cfg.StrategyVersion, wallet, marketID, outcomeToken, side, bucketStart)
	}
}

// categoryLabelByID returns the category label for the registry id, or
// "uncategorized" when the registry doesn't know it. Helper for metric
// labels in paths that don't already build a CategoryRef.
func categoryLabelByID(l *Loop, cat vo.CategoryID) string {
	if cat == 0 {
		return "uncategorized"
	}
	if c, ok := l.cache.Category(cat); ok {
		if c.Label != "" {
			return c.Label
		}
	}
	return "uncategorized"
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

// newWalletRef evaluates the Strategy-B context booster: wallet age and
// trade-count are read against the configured thresholds; a wallet that
// trips EITHER gate is flagged as new. Returns nil when:
//
//   - the booster is disabled (Config.NewWallet.Enabled=false),
//   - cfg.Traders is unwired (no source for FirstSeenAt),
//   - the wallet is empty or unknown to the DB.
//
// historyTrades comes from the trader-baseline stats (already fetched
// once per Observe call), so this helper does ONE extra DB call per
// scored trade — the GetByWallet primary-key lookup on
// polymarket_traders.
//
// Surveillance read: a wallet first seen 4 hours ago placing a $10k bet
// at 10x odds is a stronger informed-flow shape than the same trade
// from a 3-year-old wallet with thousands of trades. The booster
// attaches a reason code so the operator sees the shape at a glance.
func (l *Loop) newWalletRef(ctx context.Context, wallet string, historyTrades int) *anomaly.NewWalletRef {
	if !l.cfg.NewWallet.Enabled || l.cfg.Traders == nil || wallet == "" {
		return nil
	}
	tr, err := l.cfg.Traders.GetByWallet(ctx, wallet)
	if err != nil {
		// ErrTraderNotFound and any transient error degrade silently —
		// the booster is informational, the alert must still fire.
		return nil
	}
	now := l.now()
	age := now.Sub(tr.FirstSeenAt)
	ageGate := l.cfg.NewWallet.MaxAge > 0 && age <= l.cfg.NewWallet.MaxAge
	historyGate := l.cfg.NewWallet.MaxHistoryTrades > 0 && historyTrades <= l.cfg.NewWallet.MaxHistoryTrades
	if !ageGate && !historyGate {
		return nil
	}
	return &anomaly.NewWalletRef{
		FirstSeenAt:   tr.FirstSeenAt,
		AgeAtTrade:    age,
		HistoryTrades: int64(historyTrades),
		IsNew:         true,
	}
}

// dormantWalletRef returns the dormant-wallet booster payload when:
//
//   - DormantWallet config is Enabled and TraderActivity is wired,
//   - wallet has a non-nil trader_id in the DB,
//   - the wallet is NOT new (the new-wallet booster takes priority),
//   - the wallet's last trade before this one is older than MinIdle,
//   - the current trade clears MinNotionalUSD.
//
// Returns nil otherwise — never errors. Context booster only.
func (l *Loop) dormantWalletRef(ctx context.Context, wallet string, notionalUSD float64, tradedAt time.Time, nw *anomaly.NewWalletRef) *anomaly.DormantWalletRef {
	if !l.cfg.DormantWallet.Enabled || l.cfg.TraderActivity == nil || wallet == "" {
		return nil
	}
	if nw != nil && nw.IsNew {
		// Don't double-stamp; new-wallet booster already covers this.
		return nil
	}
	if l.cfg.DormantWallet.MinNotionalUSD > 0 && notionalUSD < l.cfg.DormantWallet.MinNotionalUSD {
		return nil
	}
	tid := l.resolveTraderID(ctx, wallet)
	if tid == nil {
		return nil
	}
	last, err := l.cfg.TraderActivity.TraderLastSeenBefore(ctx, *tid, tradedAt)
	if err != nil || last.IsZero() {
		return nil
	}
	idle := tradedAt.Sub(last)
	if l.cfg.DormantWallet.MinIdle > 0 && idle < l.cfg.DormantWallet.MinIdle {
		return nil
	}
	return &anomaly.DormantWalletRef{LastSeenAt: last, IdleDuration: idle}
}

// ownershipFusionRef computes the trade-flow ownership snapshot for
// the same (wallet, market, outcome) and returns an OwnershipRef when
// the wallet's net BUY-share count is non-trivial. Unlike the
// standalone KindOwnership emission path, fusion always renders — it
// is context attached to a primary alert, not a gating decision.
// Returns nil when the lookup is unwired or returns zero state.
func (l *Loop) ownershipFusionRef(ctx context.Context, wallet string, mid vo.MarketID, token vo.TokenID, priceHint float64) *anomaly.OwnershipRef {
	if l.cfg.OwnershipShares == nil || wallet == "" {
		return nil
	}
	tid := l.resolveTraderID(ctx, wallet)
	if tid == nil {
		return nil
	}
	dbMID := l.resolveMarketID(ctx, mid)
	if dbMID == nil {
		return nil
	}
	row, err := l.cfg.OwnershipShares.OwnershipShares(ctx, repository.OwnershipSharesQuery{
		TraderID:     *tid,
		MarketID:     *dbMID,
		OutcomeToken: string(token),
	})
	if err != nil {
		return nil
	}
	net := row.WalletBuyShares - row.WalletSellShares
	if net <= 0 || row.MarketBuyShares <= 0 {
		return nil
	}
	pct := 100 * net / row.MarketBuyShares
	if pct <= 0 {
		return nil
	}
	return &anomaly.OwnershipRef{
		Wallet:            wallet,
		MarketID:          string(mid),
		OutcomeToken:      string(token),
		SharePct:          pct,
		WalletNetShares:   net,
		MarketTotalShares: row.MarketBuyShares,
		NotionalUSD:       net * priceHint,
		Approximate:       true,
	}
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
	if c, ok := l.cache.Category(cat); ok {
		return l.cfg.Filter.Allowed(c.Slug, c.Label)
	}
	return true
}

func (l *Loop) categoryRef(cat vo.CategoryID) anomaly.CategoryRef {
	ref := anomaly.CategoryRef{ID: cat}
	if c, ok := l.cache.Category(cat); ok {
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

// Run blocks until ctx is cancelled. Detection is synchronous in Observe;
// the only periodic work is the baseline-buckets gauge (operator-visible
// counter used by Grafana). Removed in v4 cleanup: the per-market
// supporting gauges (WindowTradeRate/NotionalRate/AvgSize) that fed off
// the in-memory aggregate engine — they were bucket-only diagnostics and
// are now replaced by Postgres-derived Grafana queries.
func (l *Loop) Run(ctx context.Context) error {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.metrics.BaselineBuckets.Set(float64(l.baseline.Buckets()))
		}
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
