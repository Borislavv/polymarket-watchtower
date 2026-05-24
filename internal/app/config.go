package app

import (
	"fmt"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-playground/validator"
)

type Environment string

const (
	EnvDev   Environment = "dev"
	EnvLocal Environment = "local"
	EnvProd  Environment = "prod"
)

// Production persistence model (Strategy v4):
//
//   - When POSTGRES_DSN is set, the baseline is sourced from Postgres
//     unconditionally. There is no runtime knob that flips this off — the
//     detector queries dbbaseline.Provider, all trades are persisted by
//     persist.Sink + backfill.Worker, and alerts are deduped via
//     polymarket_alerts.dedup_key.
//   - When POSTGRES_DSN is empty, the binary runs a dev/debug mode using
//     the in-memory reservoir embedded in baseline.Baseline. This mode
//     loses state on restart, does no dedup, and is unsuitable for
//     production. Boot logs make this loud.
//
// Strategy v3 used a `BASELINE_SOURCE` env switch (postgres|memory). That
// switch is removed — it created two production modes and a configuration
// way to silently disable Postgres-backed decisions. The decision is now
// implicit from DSN presence.

// ApplicationConfig holds process-wide settings.
type ApplicationConfig struct {
	Env                 Environment   `env:"APP_ENV" envDefault:"dev" validate:"required,oneof=dev local prod"`
	LogLevel            string        `env:"LOG_LEVEL" envDefault:"info" validate:"oneof=trace debug info warn error fatal"`
	MetricsPort         int           `env:"METRICS_PORT" envDefault:"9090" validate:"required,gte=1,lte=65535"`
	ShutdownGracePeriod time.Duration `env:"SHUTDOWN_GRACE_PERIOD" envDefault:"15s" validate:"required"`
}

// PostgresConfig wires the optional PostgreSQL persistence layer. When DSN
// is empty the app stays purely in-memory (Phase-1 mode); otherwise the
// app opens a pool at startup and runs the embedded migrations before any
// worker starts.
//
// Tuning notes:
//   - MaxOpenConns bounds the upstream pool size. Postgres connection
//     overhead is non-trivial; over-provisioning hurts more than it helps.
//   - MaxIdleConns ≤ MaxOpenConns; setting them equal keeps the pool warm
//     for spiky workloads.
//   - ConnMaxLifetime forces periodic rotation so a long-running pool
//     doesn't hold a single TCP connection past upstream timeouts.
type PostgresConfig struct {
	DSN             string        `env:"POSTGRES_DSN"`
	MaxOpenConns    int           `env:"POSTGRES_MAX_OPEN_CONNS" envDefault:"10" validate:"gte=1"`
	MaxIdleConns    int           `env:"POSTGRES_MAX_IDLE_CONNS" envDefault:"5"  validate:"gte=0"`
	ConnMaxLifetime time.Duration `env:"POSTGRES_CONN_MAX_LIFETIME" envDefault:"30m" validate:"gte=0"`
	// AutoMigrate runs the embedded `db/migrations` set on startup. Leave
	// true in dev/local; flip false in production if you manage migrations
	// via a separate deploy step.
	AutoMigrate bool `env:"POSTGRES_AUTO_MIGRATE" envDefault:"true"`
}

// BackfillConfig tunes the BackfillWorker. The worker is enabled whenever
// Postgres is configured — backfill is the only way to populate the DB
// with the historical trades the detector relies on.
//
// Backfill exhausts every market's available upstream history: pagination
// walks offset 0..N until the Data API returns an empty page (status
// `completed`) or the documented 3000-row offset cap (`partial_api_limit`).
// There is no shallow-lookback short-circuit; the goal is the deepest DB
// history Polymarket allows.
type BackfillConfig struct {
	// Interval is the tick cadence; each tick claims up to Workers
	// markets and runs a full backfill pass per market.
	Interval time.Duration `env:"BACKFILL_INTERVAL" envDefault:"1m" validate:"required"`
	// Workers is the number of parallel backfills (also the per-tick
	// claim count — markets and goroutines are 1:1). Default 48; tune
	// down only to reduce upstream pressure during incidents.
	Workers int `env:"BACKFILL_WORKERS" envDefault:"48" validate:"gte=1"`
	// PageLimit is the Data API page size (max 500).
	PageLimit int `env:"BACKFILL_PAGE_LIMIT" envDefault:"500" validate:"gte=1,lte=500"`
	// StaleAfter requeues 'running' markets older than this — used to
	// recover from a crashed previous process.
	StaleAfter time.Duration `env:"BACKFILL_STALE_AFTER" envDefault:"15m" validate:"required"`
	// PartialRetryAfter is the cooldown applied to markets stamped
	// `partial_api_limit` before they become re-claimable. The 3000-
	// row offset cap is a structural Polymarket limit: re-running a
	// market within minutes will hit the same cap and burn API
	// quota for nothing. Default 6h gives the upstream enough time
	// for the documented cap to potentially shift (e.g. Polymarket
	// raises the offset cap) without keeping the markets in an
	// infinite tight retry loop.
	PartialRetryAfter time.Duration `env:"BACKFILL_PARTIAL_RETRY_AFTER" envDefault:"6h" validate:"required"`
}

// MarketSanityConfig tunes the soft-delete reaper (sanity worker). The
// worker runs hourly, finds markets that have been soft-deleted longer
// than Retention, re-checks the current upstream state, and either
// resumes (clears deleted_at, re-queues backfill) or purges (stamps
// purged_at, retains trades).
type MarketSanityConfig struct {
	Interval  time.Duration `env:"MARKET_SANITY_INTERVAL" envDefault:"1h" validate:"required"`
	Retention time.Duration `env:"MARKET_SOFT_DELETE_RETENTION" envDefault:"720h" validate:"required"`
	// ClaimLimit caps the per-tick batch. Defaults to 256 inside the
	// worker; surfaced here for operational tuning.
	ClaimLimit int `env:"MARKET_SANITY_CLAIM_LIMIT" envDefault:"256" validate:"gte=1"`
}

// OutcomesConfig tunes the post-alert resolution tracker. The worker
// runs periodically, picks sent alerts whose markets may be resolved
// upstream, and stamps a verdict (resolved_correct / resolved_wrong /
// unknown / unavailable) on each row. Signal-quality measurement only —
// never re-emits alerts, never reverses dedup.
type OutcomesConfig struct {
	Enabled    bool          `env:"OUTCOMES_ENABLED" envDefault:"true"`
	Interval   time.Duration `env:"OUTCOMES_INTERVAL" envDefault:"15m" validate:"required"`
	ClaimLimit int           `env:"OUTCOMES_CLAIM_LIMIT" envDefault:"64" validate:"gte=1"`
	// WinningPriceThreshold is the price above which an outcome token is
	// treated as the winner. Polymarket resolutions are typically
	// {1.0, 0.0}; 0.99 catches the canonical case while ignoring late-
	// trading noise around the resolution moment.
	WinningPriceThreshold float64 `env:"OUTCOMES_WINNING_PRICE_THRESHOLD" envDefault:"0.99" validate:"gt=0,lt=1"`
}

// DriftConfig tunes the CLV-lite post-trade drift enrichment worker.
// For each sent alert it computes the favourable price drift at four
// reference windows (15m / 1h / 6h / 24h) using public trade data as
// the reference price source. Sign convention: positive = favourable
// for the alert direction.
type DriftConfig struct {
	Enabled    bool          `env:"DRIFT_ENABLED" envDefault:"true"`
	Interval   time.Duration `env:"DRIFT_INTERVAL" envDefault:"5m" validate:"required"`
	ClaimLimit int           `env:"DRIFT_CLAIM_LIMIT" envDefault:"64" validate:"gte=1"`
}

// AlertSenderConfig tunes the alert sender worker that drains
// polymarket_alerts and delivers each row to Telegram.
type AlertSenderConfig struct {
	// Interval is the claim cadence.
	Interval time.Duration `env:"ALERT_SENDER_INTERVAL" envDefault:"5s" validate:"required"`
	// Workers is the number of parallel sender goroutines.
	Workers int `env:"ALERT_SENDER_WORKERS" envDefault:"1" validate:"gte=1"`
	// ClaimLimit caps the per-tick batch size pulled by ClaimPending.
	ClaimLimit int32 `env:"ALERT_CLAIM_LIMIT" envDefault:"16" validate:"gte=1"`
	// StaleSendingAfter is the recovery window for the transient `sending`
	// status. A row stuck in `sending` longer than this is reset back to
	// `pending` so the next tick re-issues it.
	StaleSendingAfter time.Duration `env:"ALERT_SENDER_STALE_AFTER" envDefault:"5m" validate:"required"`

	// Retry policy for failed delivery attempts. When enabled, the claim
	// query also picks up `status='failed' AND next_retry_at <= now()`
	// rows and re-attempts delivery; the backoff grows exponentially from
	// RetryInitialBackoff up to RetryMaxBackoff with ±RetryJitterFraction
	// jitter; after RetryMaxAttempts (counting the initial attempt) the
	// row stays in 'failed' forever (operator must intervene).
	//
	// Permanent failures (Telegram HTML parse error, "chat not found",
	// payload render error) are NEVER retried regardless of policy —
	// retrying would just burn quota.
	RetryEnabled        bool          `env:"ALERT_RETRY_ENABLED" envDefault:"true"`
	RetryMaxAttempts    int           `env:"ALERT_RETRY_MAX_ATTEMPTS" envDefault:"5" validate:"gte=1"`
	RetryInitialBackoff time.Duration `env:"ALERT_RETRY_INITIAL_BACKOFF" envDefault:"30s" validate:"required"`
	RetryMaxBackoff     time.Duration `env:"ALERT_RETRY_MAX_BACKOFF" envDefault:"30m" validate:"required"`
	RetryJitterFraction float64       `env:"ALERT_RETRY_JITTER_FRACTION" envDefault:"0.2" validate:"gte=0,lte=1"`
}

// Enabled reports whether the Postgres layer is configured. The rest of
// the app treats "no DSN" as Phase-1 mode and skips DB-touching workers.
func (c PostgresConfig) Enabled() bool { return c.DSN != "" }

// PolymarketConfig points at upstream APIs.
type PolymarketConfig struct {
	GammaURL      string        `env:"GAMMA_API_URL" envDefault:"https://gamma-api.polymarket.com" validate:"required,url"`
	DataAPIURL    string        `env:"DATA_API_URL" envDefault:"https://data-api.polymarket.com" validate:"required,url"`
	CLOBURL       string        `env:"CLOB_API_URL" envDefault:"https://clob.polymarket.com" validate:"required,url"`
	HTTPTimeout   time.Duration `env:"POLYMARKET_HTTP_TIMEOUT" envDefault:"15s" validate:"required"`
	UserAgent     string        `env:"POLYMARKET_USER_AGENT" envDefault:"polymarket-watchtower/0.1"`
	PublicBaseURL string        `env:"POLYMARKET_PUBLIC_BASE_URL" envDefault:"https://polymarket.com"`
}

// RateLimitConfig holds per-host caps.
type RateLimitConfig struct {
	GammaPerSec   float64 `env:"RL_GAMMA_PER_SEC" envDefault:"20" validate:"gt=0"`
	GammaBurst    int     `env:"RL_GAMMA_BURST" envDefault:"40" validate:"gt=0"`
	DataAPIPerSec float64 `env:"RL_DATAAPI_PER_SEC" envDefault:"14" validate:"gt=0"`
	DataAPIBurst  int     `env:"RL_DATAAPI_BURST" envDefault:"28" validate:"gt=0"`
}

// PipelineConfig drives discovery and collection cadence and scope.
//
// DiscoverySafetyMaxMarkets is an OPERATIONAL EMERGENCY CAP only. With
// Postgres as the source of truth, the production system processes every
// market that survives the category whitelist; backpressure comes from
// rate limits and DB state, not from arbitrary row truncation. The cap
// exists so an operator can quickly bound upstream load if a Polymarket
// listing spike (or a bug) tries to enroll an unreasonable number of
// markets. Default 0 means "unlimited" and is the only correct setting
// for normal production.
type PipelineConfig struct {
	DiscoverInterval          time.Duration `env:"DISCOVER_INTERVAL" envDefault:"10m" validate:"required"`
	CollectInterval           time.Duration `env:"COLLECT_INTERVAL" envDefault:"60s" validate:"required"`
	DiscoverySafetyMaxMarkets int           `env:"DISCOVERY_SAFETY_MAX_MARKETS" envDefault:"0" validate:"gte=0"`
	ActiveOnly                bool          `env:"ACTIVE_ONLY" envDefault:"true"`
	OrderBy                   string        `env:"DISCOVER_ORDER" envDefault:"volume_24hr"`
	CollectConcurrency        int           `env:"COLLECT_CONCURRENCY" envDefault:"8" validate:"gte=1"`
	// CollectBootstrapLookback is the per-market initial trade lookback
	// on first sight. Used when no persisted cursor exists yet (e.g. a
	// freshly-discovered market on a fresh database).
	CollectBootstrapLookback time.Duration `env:"COLLECT_BOOTSTRAP_LOOKBACK" envDefault:"24h" validate:"required"`
}

// CategoryFilterConfig selects which Polymarket categories the watchtower
// monitors. The filter is a WHITELIST: only categories whose
// `slug + " " + label` contains at least one whitelist token (case-
// insensitive substring) are processed. Everything else is ignored —
// discovery skips it, no backfill is scheduled, no trades are collected,
// no alerts fire.
//
// Matching is against category identity only. Market titles, event slugs,
// market slugs, and tags are NOT consulted. A sports-themed market filed
// under a whitelisted non-sports category (e.g. a FIFA question inside
// Politics) is still analysed normally.
//
// An empty whitelist disables the filter (every category passes). The
// shipped default is "Politics" — narrow on purpose so initial DB volume
// and Polymarket API load stay manageable.
type CategoryFilterConfig struct {
	Whitelist []string `env:"CATEGORY_WHITELIST" envSeparator:"," envDefault:"Politics"`
}

// AnomalyConfig encodes the per-trade `single_cluster` detector and the
// category-cluster (HARD) alert. Single-trade scoring is conservative-MIN
// of two 3-rung ladders; the cluster fires HARD when several already-firing
// single-trade alerts converge on one category in a short window.
//
//	            absolute (notional AND odds)   multiplier ladder
//	Info        $10k  AND odds 3               ≥ 100×
//	Warning     $25k  AND odds 5               ≥ 1000×
//	Critical    $100k AND odds 8               ≥ 10000×
//
// Final severity is the lower of the two tiers. Either side below Info ⇒ no
// alert. Single-trade severity caps at Critical; HARD is reserved for
// cluster alerts (multiple sharks converging).
//
// v4 cleanup removed the legacy `volume` mode and the in-memory aggregate
// engine. `single_cluster` is now the only production strategy and is
// wired unconditionally — the ANOMALY_MODE env var is gone, the
// `AggregateConfig` block (`AGG_BUCKET`, `AGG_BASELINE_WINDOW`,
// `AGG_RECENT_WINDOWS`) is gone, and `BASELINE_MAX_SAMPLES` is no longer
// an operator-facing env (it has a hardcoded default inside the
// in-memory dev-only fallback).
type AnomalyConfig struct {
	// Single-trade severity ladders (v5 tail+payoff strategy).
	//
	// A trade fires at the highest tier whose every configured gate
	// clears: absolute (notional + odds), payoff (profit if win), market
	// tail (notional / market.p95 or .p99), and trader tail
	// (notional / trader.p95 or .p99). Median multipliers are no longer
	// deciding gates — see internal/app/usecase/analytics/score doc.
	InfoMinNotionalUSD     float64 `env:"ALERT_INFO_MIN_NOTIONAL_USD" envDefault:"10000" validate:"gte=0"`
	InfoMinOdds            float64 `env:"ALERT_INFO_MIN_ODDS" envDefault:"3" validate:"gte=1"`
	WarningMinNotionalUSD  float64 `env:"ALERT_WARNING_MIN_NOTIONAL_USD" envDefault:"25000" validate:"gte=0"`
	WarningMinOdds         float64 `env:"ALERT_WARNING_MIN_ODDS" envDefault:"5" validate:"gte=1"`
	CriticalMinNotionalUSD float64 `env:"ALERT_CRITICAL_MIN_NOTIONAL_USD" envDefault:"100000" validate:"gte=0"`
	CriticalMinOdds        float64 `env:"ALERT_CRITICAL_MIN_ODDS" envDefault:"8" validate:"gte=1"`

	// Payoff floor — profit if win = notional × (odds − 1). 0 disables
	// the gate for that tier. Filters "big bet at fair odds" shapes.
	InfoMinProfitUSD     float64 `env:"ALERT_INFO_MIN_PROFIT_USD" envDefault:"5000" validate:"gte=0"`
	WarningMinProfitUSD  float64 `env:"ALERT_WARNING_MIN_PROFIT_USD" envDefault:"15000" validate:"gte=0"`
	CriticalMinProfitUSD float64 `env:"ALERT_CRITICAL_MIN_PROFIT_USD" envDefault:"50000" validate:"gte=0"`

	// Market-tail floors — required ratio of notional / market.pXX. 0
	// disables the gate. Enforced only when the market baseline is ready.
	InfoMinMarketP95Ratio     float64 `env:"ALERT_INFO_MIN_MARKET_P95_RATIO" envDefault:"1" validate:"gte=0"`
	WarningMinMarketP95Ratio  float64 `env:"ALERT_WARNING_MIN_MARKET_P95_RATIO" envDefault:"2" validate:"gte=0"`
	CriticalMinMarketP95Ratio float64 `env:"ALERT_CRITICAL_MIN_MARKET_P95_RATIO" envDefault:"4" validate:"gte=0"`
	InfoMinMarketP99Ratio     float64 `env:"ALERT_INFO_MIN_MARKET_P99_RATIO" envDefault:"0" validate:"gte=0"`
	WarningMinMarketP99Ratio  float64 `env:"ALERT_WARNING_MIN_MARKET_P99_RATIO" envDefault:"0" validate:"gte=0"`
	CriticalMinMarketP99Ratio float64 `env:"ALERT_CRITICAL_MIN_MARKET_P99_RATIO" envDefault:"0" validate:"gte=0"`

	// Trader-tail floors — required ratio of notional / trader.pXX. 0
	// disables. Enforced only when the trader baseline is ready (count
	// ≥ SingleMinBaselineTrades AND trader.P95 > 0). The detector's
	// MinTraderHistoryTrades gate is applied upstream of the scorer.
	InfoMinTraderP95Ratio     float64 `env:"ALERT_INFO_MIN_TRADER_P95_RATIO" envDefault:"1" validate:"gte=0"`
	WarningMinTraderP95Ratio  float64 `env:"ALERT_WARNING_MIN_TRADER_P95_RATIO" envDefault:"1.5" validate:"gte=0"`
	CriticalMinTraderP95Ratio float64 `env:"ALERT_CRITICAL_MIN_TRADER_P95_RATIO" envDefault:"2" validate:"gte=0"`
	InfoMinTraderP99Ratio     float64 `env:"ALERT_INFO_MIN_TRADER_P99_RATIO" envDefault:"0" validate:"gte=0"`
	WarningMinTraderP99Ratio  float64 `env:"ALERT_WARNING_MIN_TRADER_P99_RATIO" envDefault:"0" validate:"gte=0"`
	CriticalMinTraderP99Ratio float64 `env:"ALERT_CRITICAL_MIN_TRADER_P99_RATIO" envDefault:"0" validate:"gte=0"`

	// Accumulation-only multiplier floors (line total / market median).
	// Consumed by analytics/accumulation.Detector; single-trade scoring
	// ignores these. Retained as ALERT_*_MIN_MULTIPLIER for env-key
	// continuity with the v4 accumulation tuning.
	InfoMinMultiplier     float64 `env:"ALERT_INFO_MIN_MULTIPLIER" envDefault:"100" validate:"gte=0"`
	WarningMinMultiplier  float64 `env:"ALERT_WARNING_MIN_MULTIPLIER" envDefault:"1000" validate:"gte=0"`
	CriticalMinMultiplier float64 `env:"ALERT_CRITICAL_MIN_MULTIPLIER" envDefault:"10000" validate:"gte=0"`

	// v6 low-baseline confidence behaviour. When a single-trade fires
	// through an unenforced market tail gate (baseline not ready), cap
	// severity at LowBaselineSingleMaxSeverity unless the trade clears
	// the Critical absolute floor AND LowBaselineAllowCriticalAbsolute
	// is true. Prevents thin-market noise reaching pager band.
	LowBaselineCapEnabled            bool   `env:"LOW_BASELINE_CAP_ENABLED" envDefault:"true"`
	LowBaselineSingleMaxSeverity     string `env:"LOW_BASELINE_SINGLE_MAX_SEVERITY" envDefault:"info"`
	LowBaselineAllowCriticalAbsolute bool   `env:"LOW_BASELINE_ALLOW_CRITICAL_ABSOLUTE" envDefault:"true"`

	// Dormant-wallet booster: a non-new wallet that has been idle for
	// at least MinIdle and now places a trade ≥ MinNotional gets
	// DORMANT_WALLET_REVIVAL stamped on the Finding. Context only;
	// never fires standalone.
	DormantWalletEnabled        bool          `env:"DORMANT_WALLET_ENABLED" envDefault:"true"`
	DormantWalletMinIdle        time.Duration `env:"DORMANT_WALLET_MIN_IDLE" envDefault:"720h" validate:"gte=0"`
	DormantWalletMinNotionalUSD float64       `env:"DORMANT_WALLET_MIN_NOTIONAL_USD" envDefault:"5000" validate:"gte=0"`

	// New-wallet booster size floors (existing booster, just new env
	// keys per the v6 spec — single-trade and accumulation entry
	// points).
	NewWalletMinSingleNotionalUSD float64 `env:"NEW_WALLET_MIN_SINGLE_NOTIONAL_USD" envDefault:"3000" validate:"gte=0"`
	NewWalletMinLineTotalUSD      float64 `env:"NEW_WALLET_MIN_LINE_TOTAL_USD" envDefault:"10000" validate:"gte=0"`

	// Baseline shape. Every valid trade enters the reservoir — there is no
	// per-trade size filter. Readiness gates below protect against thin or
	// all-dust baselines.
	SingleMinBaselineTrades      int     `env:"SINGLE_MIN_BASELINE_TRADES" envDefault:"20" validate:"gte=0"`
	SingleMinBaselineNotionalUSD float64 `env:"SINGLE_MIN_BASELINE_NOTIONAL_USD" envDefault:"1000" validate:"gte=0"`
	// BaselineWindow is the MAXIMUM lookback the reservoir keeps; 0 means
	// "no upper bound" (only the per-bucket MaxSamples ring caps memory).
	// It is NOT a minimum-age requirement on the market — a 1-month-old
	// market with BASELINE_WINDOW=1y uses the 1 month of available history.
	BaselineWindow time.Duration `env:"BASELINE_WINDOW" envDefault:"8760h" validate:"gte=0"`
	// BaselineMinReadySpan requires the observed baseline span (newest minus
	// oldest sample) to clear this floor before alerts can fire. Distinct
	// from BaselineWindow which is a *cap*. 0 disables.
	BaselineMinReadySpan time.Duration `env:"BASELINE_MIN_READY_WINDOW" envDefault:"24h" validate:"gte=0"`

	// Lifecycle gating: only alert when the market is in the last
	// (100 - LifecycleAlertFromPct)% of its lifetime. Markets with missing
	// start/end dates are ALWAYS silenced — there is no env override (the
	// previous `ALLOW_UNKNOWN_MARKET_LIFECYCLE` was removed in the v4
	// hardening pass; an alert without lifecycle context is structurally
	// unsafe and we never want a config knob to flip that off).
	LifecycleAlertFromPct float64       `env:"LIFECYCLE_ALERT_FROM_PCT" envDefault:"75" validate:"gte=0,lte=100"`
	LifecycleHotFromPct   float64       `env:"LIFECYCLE_HOT_FROM_PCT" envDefault:"90" validate:"gte=0,lte=100"`
	MarketMinAge          time.Duration `env:"MARKET_MIN_AGE" envDefault:"24h" validate:"gte=0"`

	// LiveAlertMaxLag — defence against detect.Observe accidentally
	// firing on a backfilled trade. When trade.traded_at is older
	// than now() − this lag at Observe time, the detector skips
	// scoring and increments
	// watchtower_trades_skipped_detection_total{reason="too_old_for_live_alert"}.
	// 0 disables the gate (legacy behaviour). Default 1h matches
	// the collect tick budget — anything older is almost certainly
	// a replay.
	LiveAlertMaxLag time.Duration `env:"LIVE_ALERT_MAX_LAG" envDefault:"1h" validate:"gte=0"`

	// Trader-history multiplier (v2). Scoring adds a second multiplier:
	// notional / wallet's median historical trade. A trade fires when it
	// is anomalous on EITHER the market axis or the trader axis (the
	// surveillance literature treats them as complementary signals).
	//
	// TraderBaselineWindow caps the lookback over the wallet's stored
	// history; 0 means "no upper bound" (use the wallet's full history).
	// MinTraderHistoryTrades is the count gate: below this count the
	// trader axis is silently disabled and v1 market-only scoring applies.
	TraderBaselineWindow   time.Duration `env:"TRADER_BASELINE_WINDOW" envDefault:"2160h" validate:"gte=0"`
	MinTraderHistoryTrades int           `env:"TRADER_MIN_HISTORY_TRADES" envDefault:"5" validate:"gte=0"`

	// --- v10.8 concentration / escalation gate ---
	// The audit found 17/57 alerts (30%) came from a single event +
	// one wallet generated 4 same-event alerts in 17 minutes. These
	// knobs throttle that pattern at the alert-persistence layer:
	//   * EventAlertConcentrationLimit: alerts allowed per event in
	//     EventAlertConcentrationWindow before escalation kicks in.
	//   * RepeatedEventThresholdMultiplier: the (limit+1)th alert
	//     must clear `prev_event_max_notional * multiplier`.
	//   * WalletAlertCooldown: per-(wallet,event) cooldown.
	//   * AccumulationEscalationFactor: subsequent same-wallet
	//     alerts in the cooldown need `prev_notional * factor` to
	//     pass.
	// Set EventAlertConcentrationLimit=0 to disable the gate.
	EventAlertConcentrationLimit     int           `env:"EVENT_ALERT_CONCENTRATION_LIMIT" envDefault:"3" validate:"gte=0,lte=100"`
	EventAlertConcentrationWindow    time.Duration `env:"EVENT_ALERT_CONCENTRATION_WINDOW" envDefault:"24h" validate:"gt=0"`
	RepeatedEventThresholdMultiplier float64       `env:"REPEATED_EVENT_THRESHOLD_MULTIPLIER" envDefault:"2.0" validate:"gte=1"`
	WalletAlertCooldown              time.Duration `env:"WALLET_ALERT_COOLDOWN" envDefault:"6h" validate:"gt=0"`
	AccumulationEscalationFactor     float64       `env:"ACCUMULATION_ESCALATION_FACTOR" envDefault:"2.0" validate:"gte=1"`

	// Market-maker / arbitrage suppression (v2). When a wallet has been
	// running balanced two-sided BUY+SELL activity on the same (market,
	// outcome) over the lookback, single-trade alerts on that wallet are
	// suppressed — it is almost certainly a liquidity provider or
	// arbitrageur, not an informed-flow candidate. Cluster alerts are
	// unaffected (they have their own gates).
	//
	// MMFilterEnabled is the master switch. MMLookback is the activity
	// window. MMMinTradesPerSide is the minimum BUY and minimum SELL trade
	// count required before two-sided classification can apply (guards
	// against thin/noisy histories). MMNeutralityTol is the maximum
	// allowed |buy − sell| / max(buy, sell) — at or below this the book
	// is "balanced enough" to suppress; above this the bias is
	// directional and the alert passes through.
	MMFilterEnabled    bool          `env:"MM_FILTER_ENABLED" envDefault:"true"`
	MMLookback         time.Duration `env:"MM_LOOKBACK" envDefault:"24h" validate:"gte=0"`
	MMMinTradesPerSide int           `env:"MM_MIN_TRADES_PER_SIDE" envDefault:"4" validate:"gte=1"`
	MMNeutralityTol    float64       `env:"MM_NEUTRALITY_TOL" envDefault:"0.3" validate:"gte=0,lte=1"`

	// Same-trader accumulation-line detection (Strategy v4). The signal
	// is: one wallet repeatedly building exposure on a single
	// (market, outcome, side) inside the accumulation window. Severity
	// is anchored on the existing Info/Warning/Critical thresholds — no
	// parallel threshold universe. Two size paths can qualify a tier:
	//
	//   meaningful  : medianTrade ≥ AccumulationMinTradeFraction × tier_notional
	//                 AND lineTotal ≥ AccumulationTotalMultiplier × tier_notional
	//   many-smalls : lineTotal ≥ AccumulationManySmallsMultiplier × tier_notional
	//
	// Plus, all tiers require:
	//   trades ≥ tier_min_trades(T)        (Info=3, Warning=4, Critical=5)
	//   avg_odds ≥ T.MinOdds
	//   lineTotal / marketMedian ≥ T.MinMultiplier
	//
	// Hard accumulation: trades ≥ 5 AND lineTotal ≥ HardMultiplier ×
	// Critical.MinNotionalUSD AND HOT lifecycle. Reserved for rare cases
	// where a wallet is clearly accumulating into a market about to close.
	//
	// Accumulation is Postgres-only — the detector reads
	// AccumulationLineSummary from polymarket_trades.
	AccumulationEnabled              bool          `env:"ACCUMULATION_ENABLED" envDefault:"true"`
	AccumulationWindow               time.Duration `env:"ACCUMULATION_WINDOW" envDefault:"24h" validate:"gt=0"`
	AccumulationMinTrades            int           `env:"ACCUMULATION_MIN_TRADES" envDefault:"3" validate:"gte=2"`
	AccumulationMinTradeFraction     float64       `env:"ACCUMULATION_MIN_TRADE_FRACTION_OF_INFO" envDefault:"0.6" validate:"gt=0,lte=1"`
	AccumulationTotalMultiplier      float64       `env:"ACCUMULATION_TOTAL_MULTIPLIER" envDefault:"2" validate:"gt=1"`
	AccumulationManySmallsMultiplier float64       `env:"ACCUMULATION_MANY_SMALLS_MULTIPLIER" envDefault:"4" validate:"gt=1"`
	AccumulationHardMultiplier       float64       `env:"ACCUMULATION_HARD_MULTIPLIER" envDefault:"3" validate:"gt=1"`
	AccumulationCooldown             time.Duration `env:"ACCUMULATION_COOLDOWN" envDefault:"30m" validate:"gt=0"`

	// Quiet-market wake-up (Strategy v4). Context detector — does NOT fire
	// alerts on its own. After a single-trade alert or accumulation line
	// alert qualifies, the detector tags it with QUIET_MARKET_WAKEUP when
	// the (market, outcome) was historically quiet AND the event is large
	// enough to constitute a wake-up. See doc/strategies/single-cluster.md.
	//
	// Ceilings + idle floor + per-event size + optional multiplier — all
	// must clear for the wake-up tag to attach.
	QuietMarketEnabled            bool          `env:"QUIET_MARKET_ENABLED" envDefault:"true"`
	QuietMarketMaxTradesPerDay    float64       `env:"QUIET_MARKET_MAX_TRADES_PER_DAY" envDefault:"10" validate:"gte=0"`
	QuietMarketMaxNotionalPerDay  float64       `env:"QUIET_MARKET_MAX_NOTIONAL_PER_DAY_USD" envDefault:"5000" validate:"gte=0"`
	QuietMarketMinIdleDuration    time.Duration `env:"QUIET_MARKET_MIN_IDLE_DURATION" envDefault:"6h" validate:"gte=0"`
	QuietMarketMinCurrentNotional float64       `env:"QUIET_MARKET_MIN_CURRENT_NOTIONAL_USD" envDefault:"10000" validate:"gte=0"`
	QuietMarketMinMultiplier      float64       `env:"QUIET_MARKET_MIN_MULTIPLIER" envDefault:"50" validate:"gte=0"`

	// Cluster (HARD) alert. Fires when several already-firing single-trade
	// alerts converge on one category within ClusterWindow.
	ClusterWindow      time.Duration `env:"CLUSTER_WINDOW" envDefault:"30m" validate:"required"`
	ClusterMinTrades   int           `env:"CLUSTER_MIN_ANOMALOUS_TRADES" envDefault:"3" validate:"gte=2"`
	ClusterMinWallets  int           `env:"CLUSTER_MIN_UNIQUE_TRADERS" envDefault:"2" validate:"gte=1"`
	ClusterMinTotalUSD float64       `env:"CLUSTER_MIN_TOTAL_NOTIONAL_USD" envDefault:"50000" validate:"gte=0"`
	ClusterCooldown    time.Duration `env:"CLUSTER_COOLDOWN" envDefault:"30m" validate:"required"`

	// New-wallet context booster (Strategy B). Attaches NEW_WALLET_*
	// reason codes to single-trade and accumulation Findings when the
	// firing wallet is "new" — first seen within MaxAge OR with ≤
	// MaxHistoryTrades stored trades. Never a standalone alert.
	NewWalletEnabled          bool          `env:"NEW_WALLET_ENABLED" envDefault:"true"`
	NewWalletMaxAge           time.Duration `env:"NEW_WALLET_MAX_AGE" envDefault:"168h" validate:"gte=0"`
	NewWalletMaxHistoryTrades int           `env:"NEW_WALLET_MAX_HISTORY_TRADES" envDefault:"10" validate:"gte=0"`

	// Ownership concentration (Strategy E). Distinct alert kind
	// `ownership_concentration`. Fired alongside the accumulation path
	// when a wallet's trade-flow share crosses a tier AND the absolute
	// position notional clears the floor. APPROXIMATION — no holders
	// endpoint is wired upstream; see
	// internal/app/usecase/analytics/ownership doc.
	OwnershipEnabled        bool    `env:"OWNERSHIP_CONCENTRATION_ENABLED" envDefault:"true"`
	OwnershipInfoPct        float64 `env:"OWNERSHIP_INFO_PCT" envDefault:"10" validate:"gt=0,lt=100"`
	OwnershipWarningPct     float64 `env:"OWNERSHIP_WARNING_PCT" envDefault:"15" validate:"gt=0,lt=100"`
	OwnershipCriticalPct    float64 `env:"OWNERSHIP_CRITICAL_PCT" envDefault:"25" validate:"gt=0,lt=100"`
	OwnershipMinNotionalUSD float64 `env:"OWNERSHIP_MIN_NOTIONAL_USD" envDefault:"10000" validate:"gte=0"`
}

// AlertingConfig wires sinks.
//
// v11.3 typed routing:
//   - TELEGRAM_CHAT_ID         — customer-facing SIGNAL chat. Only
//     real flow alerts and actionable hourly news intelligence
//     reach it.
//   - TELEGRAM_ADMIN_CHAT_ID   — internal ADMIN telemetry chat.
//     Signal-quality reports, stats, scorecards, etc. NEVER
//     receives the signal feed; admin messages NEVER fall back to
//     the signal chat when the admin destination is missing.
//
// The single *telegram.Bot client serves both chats; the
// *telegram.Router resolves chat id from the message Surface.
type AlertingConfig struct {
	WebhookURL       string        `env:"ALERT_WEBHOOK_URL"`
	TelegramEnabled  bool          `env:"TELEGRAM_ENABLED" envDefault:"false"`
	TelegramBotToken string        `env:"TELEGRAM_BOT_TOKEN"`
	TelegramChatID   string        `env:"TELEGRAM_CHAT_ID"`
	TelegramBaseURL  string        `env:"TELEGRAM_BASE_URL"`
	TelegramTimeout  time.Duration `env:"TELEGRAM_TIMEOUT" envDefault:"5s"`

	// --- v11.3 admin chat ---
	TelegramAdminEnabled       bool   `env:"TELEGRAM_ADMIN_ENABLED" envDefault:"false"`
	TelegramAdminChatID        string `env:"TELEGRAM_ADMIN_CHAT_ID"`
	TelegramAllowSameChatAdmin bool   `env:"TELEGRAM_ALLOW_SAME_CHAT_FOR_ADMIN" envDefault:"false"`

	// Per-surface admin toggles. Each one gates a specific admin
	// surface even when the admin chat is configured; useful for
	// rolling new admin reports one at a time.
	TelegramAdminSignalQualityReports bool `env:"TELEGRAM_ADMIN_SIGNAL_QUALITY_REPORTS_ENABLED" envDefault:"false"`
	TelegramAdminStats                bool `env:"TELEGRAM_ADMIN_STATS_ENABLED" envDefault:"false"`
	TelegramAdminStrategyScorecard    bool `env:"TELEGRAM_ADMIN_STRATEGY_SCORECARD_ENABLED" envDefault:"false"`
	TelegramAdminOperationalHealth    bool `env:"TELEGRAM_ADMIN_OPERATIONAL_HEALTH_ENABLED" envDefault:"false"`
	TelegramAdminBudgetReports        bool `env:"TELEGRAM_ADMIN_BUDGET_REPORTS_ENABLED" envDefault:"false"`
	TelegramAdminSuppressionReports   bool `env:"TELEGRAM_ADMIN_SUPPRESSION_REPORTS_ENABLED" envDefault:"false"`

	// --- v11.4 bounded signal-quality report ---
	// SIGNAL_QUALITY_LOOKBACK is the default fallback. The
	// per-period lookbacks below are used when the worker fires
	// the Daily/Weekly/Monthly/Quarterly/Yearly variant.
	// SIGNAL_QUALITY_MAX_ALERTS caps the bounded SQL scan; when
	// eligible rows exceed it the renderer surfaces a "Scan
	// truncated" banner with the eligible count + the cap.
	SignalQualityLookback          time.Duration `env:"SIGNAL_QUALITY_LOOKBACK" envDefault:"8760h"`
	SignalQualityDailyLookback     time.Duration `env:"SIGNAL_QUALITY_DAILY_LOOKBACK" envDefault:"24h"`
	SignalQualityWeeklyLookback    time.Duration `env:"SIGNAL_QUALITY_WEEKLY_LOOKBACK" envDefault:"168h"`
	SignalQualityMonthlyLookback   time.Duration `env:"SIGNAL_QUALITY_MONTHLY_LOOKBACK" envDefault:"720h"`
	SignalQualityQuarterlyLookback time.Duration `env:"SIGNAL_QUALITY_QUARTERLY_LOOKBACK" envDefault:"2160h"`
	SignalQualityYearlyLookback    time.Duration `env:"SIGNAL_QUALITY_YEARLY_LOOKBACK" envDefault:"8760h"`
	SignalQualityMaxAlerts         int           `env:"SIGNAL_QUALITY_MAX_ALERTS" envDefault:"5000"`

	// --- v11.4 Market Close Review learning loop ---
	// Reviews recently-closed markets, asks the AI whether
	// Watchtower's alerts caught real informed flow, persists
	// the verdict + tuning recommendations, and posts a compact
	// admin Telegram body. Admin destination only. AI gated by
	// the market_close_review budget bucket.
	MarketCloseReviewEnabled                bool          `env:"MARKET_CLOSE_REVIEW_ENABLED" envDefault:"true"`
	MarketCloseReviewInterval               time.Duration `env:"MARKET_CLOSE_REVIEW_INTERVAL" envDefault:"30m"`
	MarketCloseReviewLookback               time.Duration `env:"MARKET_CLOSE_REVIEW_LOOKBACK" envDefault:"24h"`
	MarketCloseReviewMarketMaxAgeAfterClose time.Duration `env:"MARKET_CLOSE_REVIEW_MARKET_MAX_AGE_AFTER_CLOSE" envDefault:"72h"`
	MarketCloseReviewHistoryLookback        time.Duration `env:"MARKET_CLOSE_REVIEW_HISTORY_LOOKBACK" envDefault:"8760h"`
	MarketCloseReviewMinAlerts              int           `env:"MARKET_CLOSE_REVIEW_MIN_ALERTS" envDefault:"1"`
	MarketCloseReviewRequireAlertOrNews     bool          `env:"MARKET_CLOSE_REVIEW_REQUIRE_ALERT_OR_NEWS" envDefault:"true"`
	MarketCloseReviewMaxMarketsPerRun       int           `env:"MARKET_CLOSE_REVIEW_MAX_MARKETS_PER_RUN" envDefault:"10"`
	MarketCloseReviewMaxAlertsPerMarket     int           `env:"MARKET_CLOSE_REVIEW_MAX_ALERTS_PER_MARKET" envDefault:"50"`
	MarketCloseReviewMaxEventsPerMarket     int           `env:"MARKET_CLOSE_REVIEW_MAX_EVENTS_PER_MARKET" envDefault:"30"`
	MarketCloseReviewAIEnabled              bool          `env:"MARKET_CLOSE_REVIEW_AI_ENABLED" envDefault:"true"`
	MarketCloseReviewAITimeout              time.Duration `env:"MARKET_CLOSE_REVIEW_AI_TIMEOUT" envDefault:"60s"`
	MarketCloseReviewAIModel                string        `env:"MARKET_CLOSE_REVIEW_AI_MODEL" envDefault:""`
	MarketCloseReviewDailyBudgetUSD         float64       `env:"MARKET_CLOSE_REVIEW_DAILY_BUDGET_USD" envDefault:"3"`
	MarketCloseReviewSendAdminTelegram      bool          `env:"MARKET_CLOSE_REVIEW_SEND_ADMIN_TELEGRAM" envDefault:"true"`
	MarketCloseReviewSetReactions           bool          `env:"MARKET_CLOSE_REVIEW_SET_REACTIONS" envDefault:"true"`
	MarketCloseReviewReactionSuccess        string        `env:"MARKET_CLOSE_REVIEW_REACTION_SUCCESS" envDefault:"👍"`
	MarketCloseReviewReactionFailure        string        `env:"MARKET_CLOSE_REVIEW_REACTION_FAILURE" envDefault:"👎"`
	MarketCloseReviewReactionAmbiguous      string        `env:"MARKET_CLOSE_REVIEW_REACTION_AMBIGUOUS" envDefault:"🤔"`
	MarketCloseReviewReactionSkipAmbiguous  bool          `env:"MARKET_CLOSE_REVIEW_REACTION_SKIP_AMBIGUOUS" envDefault:"false"`

	// GrafanaBaseURL: empty by default to avoid shipping the
	// docker-compose default (http://localhost:3000), which renders as
	// a dead link in Telegram on mobile. Set to a host reachable from
	// alert recipients; loopback / localhost / link-local hosts are
	// silently elided from the rendered alert.
	GrafanaBaseURL string        `env:"GRAFANA_BASE_URL" envDefault:""`
	GrafanaDashUID string        `env:"GRAFANA_DASH_UID" envDefault:""`
	GrafanaContext time.Duration `env:"GRAFANA_CONTEXT_WINDOW" envDefault:"1h"`
}

// StatsReportConfig wires the optional periodic stats summary worker.
// Disabled by default; enable in production to get a "pipeline is
// alive" heartbeat in the same Telegram chat as per-alert messages.
type StatsReportConfig struct {
	Enabled      bool          `env:"TELEGRAM_STATS_ENABLED" envDefault:"false"`
	Interval     time.Duration `env:"TELEGRAM_STATS_INTERVAL" envDefault:"2h"`
	StartupGrace time.Duration `env:"TELEGRAM_STATS_STARTUP_GRACE" envDefault:"0"`
}

// TelegramReactionsConfig wires the outcome-reaction pass on the
// outcomes worker. Reactions are sent to the same chat the alerts
// were sent to (TELEGRAM_CHAT_ID); the reaction emoji is operator-
// configurable so a deployment in a channel that disabled the
// default ✅/💭 emojis can swap in something else from the allowed
// set.
type TelegramReactionsConfig struct {
	Enabled          bool   `env:"TELEGRAM_OUTCOME_REACTIONS_ENABLED" envDefault:"true"`
	SuccessEmoji     string `env:"TELEGRAM_OUTCOME_SUCCESS_REACTION" envDefault:"👍"`
	FailureEmoji     string `env:"TELEGRAM_OUTCOME_FAILURE_REACTION" envDefault:"👎"`
	AmbiguousEmoji   string `env:"TELEGRAM_OUTCOME_AMBIGUOUS_REACTION" envDefault:"🤔"`
	DisableAmbiguous bool   `env:"TELEGRAM_OUTCOME_DISABLE_AMBIGUOUS" envDefault:"false"`
}

type Config struct {
	Application       ApplicationConfig
	Postgres          PostgresConfig
	Backfill          BackfillConfig
	MarketSanity      MarketSanityConfig
	Outcomes          OutcomesConfig
	Drift             DriftConfig
	AlertSender       AlertSenderConfig
	TelegramReactions TelegramReactionsConfig
	Polymarket        PolymarketConfig
	RateLimit         RateLimitConfig
	Pipeline          PipelineConfig
	Anomaly           AnomalyConfig
	CategoryFilter    CategoryFilterConfig
	Alerting          AlertingConfig
	StatsReport       StatsReportConfig
	Detection         DetectionConfig
	AIAnalysis        AIAnalysisConfig
	EventPage         EventPageContextConfig
	Catalyst          CatalystConfig
	EventFlow         EventFlowConfig
	AIBudget          AIBudgetConfig
	AIPreflight       AIPreflightConfig
	WS                WebSocketConfig
	// Strategy is the v11.5 shadow-first detector + worker block.
	// Every nested field defaults to disabled / shadow-only so the
	// block is inert until an operator opts in.
	Strategy StrategyConfig
}

// WebSocketConfig wires the v10.4 hybrid WebSocket fast-lane.
//
// v10.6 flipped WS_ENABLED to true by default. Polling + backfill +
// alertsender remain the canonical pipeline; WS is strictly a low-
// latency trigger accelerator. Operators who don't want any WS
// traffic at all can still set WS_ENABLED=false.
//
// v11.12-insider-prior: WS subscription is MARKET-LIMITED, not
// TOKEN-LIMITED. The selector caps at WS_MAX_MARKETS and every
// selected market subscribes to ALL its outcome tokens (no silent
// slicing). WS_MAX_TOKENS_HARD_CAP is an emergency circuit-breaker
// only; when exceeded the WS client fails LOUDLY with
// ws.ErrTokenHardCapExceeded rather than silently dropping tokens.
//
// The legacy WS_MAX_TOKENS knob is in staleEnvKeys{} and boot-fails
// loudly if set — earlier it silently truncated tokens after market
// selection, masking the May/Jul/Dec multi-outcome failure mode.
//
// Safety belts:
//   - WS_MAX_MARKETS > 250 requires WS_ALLOW_LARGE_SUBSCRIPTION=true.
//   - WS_ENABLED=true requires WS_MAX_MARKETS > 0.
type WebSocketConfig struct {
	Enabled             bool   `env:"WS_ENABLED" envDefault:"true"`
	MarketStreamEnabled bool   `env:"WS_MARKET_STREAM_ENABLED" envDefault:"true"`
	SubscriptionMode    string `env:"WS_SUBSCRIPTION_MODE" envDefault:"hot"`
	MaxMarkets          int    `env:"WS_MAX_MARKETS" envDefault:"25" validate:"gte=1,lte=5000"`
	MaxTokensHardCap    int    `env:"WS_MAX_TOKENS_HARD_CAP" envDefault:"50000" validate:"gte=1,lte=200000"`
	// AllowLargeSubscription unlocks WS_MAX_MARKETS > 250. False by
	// default — a typo of WS_MAX_MARKETS=2500 now fails Validate()
	// rather than fanning out a 5000-token subscription.
	AllowLargeSubscription    bool          `env:"WS_ALLOW_LARGE_SUBSCRIPTION" envDefault:"false"`
	ReconnectMinBackoff       time.Duration `env:"WS_RECONNECT_MIN_BACKOFF" envDefault:"1s" validate:"gt=0"`
	ReconnectMaxBackoff       time.Duration `env:"WS_RECONNECT_MAX_BACKOFF" envDefault:"30s" validate:"gt=0"`
	PingInterval              time.Duration `env:"WS_PING_INTERVAL" envDefault:"10s" validate:"gt=0"`
	ReadTimeout               time.Duration `env:"WS_READ_TIMEOUT" envDefault:"45s" validate:"gt=0"`
	WriteTimeout              time.Duration `env:"WS_WRITE_TIMEOUT" envDefault:"10s" validate:"gt=0"`
	EventBuffer               int           `env:"WS_EVENT_BUFFER" envDefault:"10000" validate:"gte=100"`
	DropPolicy                string        `env:"WS_DROP_POLICY" envDefault:"drop_low_priority"`
	RawCaptureEnabled         bool          `env:"WS_RAW_CAPTURE_ENABLED" envDefault:"false"`
	RawCaptureMaxBytes        int           `env:"WS_RAW_CAPTURE_MAX_BYTES" envDefault:"4096" validate:"gte=128"`
	ReconcileEnabled          bool          `env:"WS_RECONCILE_ENABLED" envDefault:"true"`
	ReconcileInterval         time.Duration `env:"WS_RECONCILE_INTERVAL" envDefault:"2m" validate:"gt=0"`
	GapRecoveryLookback       time.Duration `env:"WS_GAP_RECOVERY_LOOKBACK" envDefault:"10m" validate:"gt=0"`
	HealthStaleAfter          time.Duration `env:"WS_HEALTH_STALE_AFTER" envDefault:"60s" validate:"gt=0"`
	StartupSubscribeDelay     time.Duration `env:"WS_STARTUP_SUBSCRIBE_DELAY" envDefault:"10s" validate:"gte=0"`
	PriceMoveTrigger          float64       `env:"WS_PRICE_MOVE_TRIGGER" envDefault:"0.03" validate:"gt=0,lte=1"`
	RepricingTriggerCooldown  time.Duration `env:"WS_REPRICING_TRIGGER_COOLDOWN" envDefault:"60s" validate:"gt=0"`
	PredictionRefreshCooldown time.Duration `env:"WS_PREDICTION_REFRESH_TRIGGER_COOLDOWN" envDefault:"5m" validate:"gt=0"`
	Endpoint                  string        `env:"WS_ENDPOINT" envDefault:"wss://ws-subscriptions-clob.polymarket.com/ws/market"`
	// v11.11 — opt-in priority 7 in hot mode. When true, markets
	// with ≥HighTradeMinTrades24h trades over HighTradeLookbackHours
	// are added to the selector universe even when they have no
	// prediction / catalyst / annotation hook. Off by default so the
	// load profile cannot regress without an explicit operator flip.
	IncludeHighTradeMarkets bool `env:"WS_INCLUDE_HIGH_TRADE_MARKETS" envDefault:"false"`
	HighTradeMinTrades24h   int  `env:"WS_HIGH_TRADE_MIN_TRADES_24H" envDefault:"50" validate:"gte=1,lte=100000"`
	HighTradeLookbackHours  int  `env:"WS_HIGH_TRADE_LOOKBACK_HOURS" envDefault:"24" validate:"gte=1,lte=168"`
	// AnnotationLookback is the freshness window for the
	// event_annotation_recent WS selector bucket. Default 168h = 7d
	// matches the live linkup-source cadence (~1 entry/day/event).
	// The previous 12h window caught 1 event vs 4 events in 7d on
	// the live database — see selector.go bucket 5.
	AnnotationLookback time.Duration `env:"WS_ANNOTATION_LOOKBACK" envDefault:"168h" validate:"gt=0"`
}

// EventFlowConfig drives the deterministic event-level flow
// aggregation used by every AI prompt that needs real Watchtower
// context (daily intel, prediction prompts). NEVER blocks alerts.
type EventFlowConfig struct {
	Enabled          bool          `env:"EVENT_FLOW_SUMMARY_ENABLED" envDefault:"true"`
	Lookback         time.Duration `env:"EVENT_FLOW_SUMMARY_LOOKBACK" envDefault:"24h" validate:"gt=0"`
	MaxAlerts        int           `env:"EVENT_FLOW_SUMMARY_MAX_ALERTS" envDefault:"25" validate:"gte=1,lte=500"`
	MaxTrades        int           `env:"EVENT_FLOW_SUMMARY_MAX_TRADES" envDefault:"150" validate:"gte=1,lte=2000"`
	MinLargeTradeUSD float64       `env:"EVENT_FLOW_SUMMARY_MIN_LARGE_TRADE_USD" envDefault:"10000" validate:"gte=0"`
	TopItems         int           `env:"EVENT_FLOW_SUMMARY_TOP_ITEMS" envDefault:"10" validate:"gte=1,lte=50"`
}

// AIPreflightConfig wires the v10.3 per-surface preflight caps.
// Every AI surface routes its prompt through aipreflight.Preflight
// which enforces these caps + the AIBudgetConfig daily budgets.
type AIPreflightConfig struct {
	MaxInputCharsAlert      int `env:"AI_MAX_INPUT_CHARS_ALERT" envDefault:"18000" validate:"gte=1000"`
	MaxInputCharsCatalyst   int `env:"AI_MAX_INPUT_CHARS_CATALYST" envDefault:"18000" validate:"gte=1000"`
	MaxOutputTokensAlert    int `env:"AI_MAX_OUTPUT_TOKENS_ALERT" envDefault:"1200" validate:"gte=200"`
	MaxOutputTokensCatalyst int `env:"AI_MAX_OUTPUT_TOKENS_CATALYST" envDefault:"1200" validate:"gte=200"`
}

// AIBudgetConfig wires the process-local AI budget governor (PART 5
// of the v10.0 operational pass). 0 on any field disables that
// specific cap; the recommended production values are the defaults
// below and live in CLAUDE.md / .env.example.
type AIBudgetConfig struct {
	GlobalDailyUSD           float64 `env:"AI_GLOBAL_DAILY_BUDGET_USD" envDefault:"25" validate:"gte=0"`
	AlertAnalysisDailyUSD    float64 `env:"AI_ANALYSIS_DAILY_BUDGET_USD_OVERRIDE" envDefault:"0" validate:"gte=0"`
	CatalystImporterDailyUSD float64 `env:"EVENT_CATALYST_IMPORTER_DAILY_BUDGET_USD" envDefault:"8" validate:"gte=0"`
}

// CatalystConfig wires the Political-Catalyst Intelligence overlay.
// Catalyst rows are stored in polymarket_event_catalysts and modify
// the interpretation of every alert (the Telegram "Blocked Alert"
// block + the prompt "Future catalysts:" slot). Reading is always
// safe: an empty table degrades to "no catalyst recorded" and the
// alert pipeline is unaffected.
type CatalystConfig struct {
	Enabled        bool `env:"CATALYST_ENABLED" envDefault:"true"`
	PromptMaxItems int  `env:"CATALYST_PROMPT_MAX_ITEMS" envDefault:"6" validate:"gte=1,lte=50"`
	PromptMaxChars int  `env:"CATALYST_PROMPT_MAX_CHARS" envDefault:"2000" validate:"gte=200,lte=20000"`

	// Importer — periodic background extractor (v9.6). When
	// enabled, the system imports / refreshes catalyst rows
	// automatically every Interval. Operator seeding is no longer
	// required. Failure NEVER blocks alert delivery.
	ImporterEnabled        bool          `env:"EVENT_CATALYST_IMPORTER_ENABLED" envDefault:"true"`
	ImporterInterval       time.Duration `env:"EVENT_CATALYST_IMPORTER_INTERVAL" envDefault:"5m" validate:"gt=0"`
	ImporterCategoryCSV    string        `env:"EVENT_CATALYST_IMPORTER_CATEGORY_WHITELIST" envDefault:"Politics,Geopolitics,Elections"`
	ImporterBatchSize      int           `env:"EVENT_CATALYST_IMPORTER_BATCH_SIZE" envDefault:"50" validate:"gte=1,lte=500"`
	ImporterConcurrency    int           `env:"EVENT_CATALYST_IMPORTER_CONCURRENCY" envDefault:"4" validate:"gte=1,lte=32"`
	ImporterLookback       time.Duration `env:"EVENT_CATALYST_IMPORTER_LOOKBACK" envDefault:"48h" validate:"gt=0"`
	ImporterAIEnabled      bool          `env:"EVENT_CATALYST_IMPORTER_AI_ENABLED" envDefault:"true"`
	ImporterAITimeout      time.Duration `env:"EVENT_CATALYST_IMPORTER_AI_TIMEOUT" envDefault:"45s" validate:"gt=0"`
	ImporterMaxAnnotations int           `env:"EVENT_CATALYST_IMPORTER_MAX_ANNOTATIONS" envDefault:"40" validate:"gte=1,lte=500"`
	ImporterMaxPromptChars int           `env:"EVENT_CATALYST_IMPORTER_MAX_PROMPT_CHARS" envDefault:"12000" validate:"gte=1000,lte=64000"`
	ImporterMinConfidence  float64       `env:"EVENT_CATALYST_IMPORTER_MIN_CONFIDENCE" envDefault:"0.55" validate:"gte=0,lte=1"`
	ImporterStaleAfter     time.Duration `env:"EVENT_CATALYST_IMPORTER_STALE_AFTER" envDefault:"168h" validate:"gt=0"`

	// --- v10.0 tiering (PART 6 of operational pass) ---
	// Per-tier cadence + threshold knobs. With TieringEnabled=true
	// the worker stops hitting every market every Interval; tier-1
	// (high-signal political races, big volume, multiple alerts)
	// stays at 5m, tier-2 at 15m, tier-3 at 60m. Cuts AI calls by
	// ~3× without losing freshness on load-bearing markets.
	ImporterTieringEnabled       bool          `env:"EVENT_CATALYST_IMPORTER_TIERING_ENABLED" envDefault:"true"`
	ImporterTier1Interval        time.Duration `env:"EVENT_CATALYST_IMPORTER_TIER1_INTERVAL" envDefault:"5m" validate:"gt=0"`
	ImporterTier2Interval        time.Duration `env:"EVENT_CATALYST_IMPORTER_TIER2_INTERVAL" envDefault:"15m" validate:"gt=0"`
	ImporterTier3Interval        time.Duration `env:"EVENT_CATALYST_IMPORTER_TIER3_INTERVAL" envDefault:"60m" validate:"gt=0"`
	ImporterTier1MinVolume24hUSD float64       `env:"EVENT_CATALYST_IMPORTER_TIER1_MIN_VOLUME_24H_USD" envDefault:"100000" validate:"gte=0"`
	ImporterTier1MinAlerts24h    int           `env:"EVENT_CATALYST_IMPORTER_TIER1_MIN_ALERTS_24H" envDefault:"3" validate:"gte=0,lte=100"`
	ImporterTier2MinVolume24hUSD float64       `env:"EVENT_CATALYST_IMPORTER_TIER2_MIN_VOLUME_24H_USD" envDefault:"10000" validate:"gte=0"`
	ImporterTier1CategoriesCSV   string        `env:"EVENT_CATALYST_IMPORTER_TIER1_CATEGORIES" envDefault:"Geopolitics,Elections"`
}

// EventPageContextConfig wires the Polymarket event-page narrative
// pipeline. Source: https://polymarket.com/_next/data/<buildId>/en/
// event/<slug>.json — hydrated Next.js payload that carries the
// curated chart annotations (queryKey ["annotations","event",<slug>])
// the Polymarket UI shows around the event chart. Failure NEVER
// blocks alert delivery; the AI prompt renders an "unavailable" slot.
type EventPageContextConfig struct {
	Enabled          bool          `env:"POLYMARKET_EVENT_PAGE_CONTEXT_ENABLED" envDefault:"true"`
	RefreshInfo      time.Duration `env:"POLYMARKET_EVENT_PAGE_REFRESH_INFO" envDefault:"10m" validate:"gt=0"`
	RefreshImportant time.Duration `env:"POLYMARKET_EVENT_PAGE_REFRESH_IMPORTANT" envDefault:"5m" validate:"gt=0"`
	RefreshHot       time.Duration `env:"POLYMARKET_EVENT_PAGE_REFRESH_HOT" envDefault:"5m" validate:"gt=0"`
	FetchTimeout     time.Duration `env:"POLYMARKET_EVENT_PAGE_TIMEOUT" envDefault:"8s" validate:"gt=0"`
	BuildIDTTL       time.Duration `env:"POLYMARKET_EVENT_PAGE_BUILD_ID_TTL" envDefault:"30m" validate:"gt=0"`
	RawJSONMaxBytes  int           `env:"POLYMARKET_EVENT_PAGE_RAW_JSON_MAX_BYTES" envDefault:"1048576" validate:"gte=4096"`
	PromptMaxItems   int           `env:"POLYMARKET_EVENT_PAGE_PROMPT_MAX_ITEMS" envDefault:"25" validate:"gte=1,lte=200"`
	PromptMaxChars   int           `env:"POLYMARKET_EVENT_PAGE_PROMPT_MAX_CHARS" envDefault:"5000" validate:"gte=200,lte=20000"`
	// HTMLBaseURL is the public Polymarket site. Override only for
	// integration tests / staging.
	HTMLBaseURL string `env:"POLYMARKET_EVENT_PAGE_HTML_BASE_URL" envDefault:"https://polymarket.com"`
	// MaxRedirects bounds the redirect chain length the v10.5 client
	// follows per fetch. Polymarket's data route emits exactly one
	// 307 today (to either /event/<slug> HTML or another _next/data
	// JSON URL); the cap is the safety belt against loops.
	MaxRedirects int `env:"EVENT_PAGE_MAX_REDIRECTS" envDefault:"5" validate:"gte=1,lte=20"`
}

// AIAnalysisConfig wires the AI market-intelligence layer. The
// service operates fully without an OpenAI key — when OPENAI_API_KEY
// is empty (or AIAnalysisEnabled=false), every analyzer call short-
// circuits to StatusSkipped and the Telegram path elides the
// Analyst-note block. No AI-related code path can ever block alert
// emission.
type AIAnalysisConfig struct {
	Enabled  bool   `env:"AI_ANALYSIS_ENABLED" envDefault:"false"`
	Provider string `env:"AI_ANALYSIS_PROVIDER" envDefault:"openai"`
	Model    string `env:"AI_ANALYSIS_MODEL" envDefault:"gpt-4.1-mini"`
	APIKey   string `env:"OPENAI_API_KEY"`
	BaseURL  string `env:"OPENAI_BASE_URL" envDefault:"https://api.openai.com/v1"`

	// 8s was the legacy default and was too aggressive in prod —
	// alert AI calls regularly landed in the 10-20s range and tripped
	// the timeout cliff. 45s aligns with the operator-spec
	// ALERT_AI_TIMEOUT default and gives the model real headroom.
	Timeout        time.Duration `env:"AI_ANALYSIS_TIMEOUT" envDefault:"45s" validate:"gt=0"`
	MaxOutputChars int           `env:"AI_ANALYSIS_MAX_OUTPUT_CHARS" envDefault:"700" validate:"gte=100,lte=4000"`
	MaxPromptChars int           `env:"AI_ANALYSIS_MAX_PROMPT_CHARS" envDefault:"2500" validate:"gte=200,lte=20000"`

	// Cost control.
	RateLimitPerMin int     `env:"AI_ANALYSIS_RATE_LIMIT_PER_MIN" envDefault:"10" validate:"gte=1"`
	DailyBudgetUSD  float64 `env:"AI_ANALYSIS_DAILY_BUDGET_USD" envDefault:"5" validate:"gt=0"`

	// Per-1k-token cost overrides — operator-tunable when model
	// pricing changes.
	PromptCostPer1kUSD     float64 `env:"AI_ANALYSIS_PROMPT_COST_PER_1K_USD" envDefault:"0.00015" validate:"gte=0"`
	CompletionCostPer1kUSD float64 `env:"AI_ANALYSIS_COMPLETION_COST_PER_1K_USD" envDefault:"0.0006" validate:"gte=0"`

	// Feature toggles.
	AlertsEnabled    bool `env:"AI_ANALYSIS_TELEGRAM_ALERTS_ENABLED" envDefault:"true"`
	LogAlertsEnabled bool `env:"AI_ANALYSIS_LOG_ALERTS_ENABLED" envDefault:"true"`
	ReportsEnabled   bool `env:"AI_ANALYSIS_REPORTS_ENABLED" envDefault:"true"`

	// WebSearchEnabled flips alert + market-report calls onto the
	// OpenAI Responses API with the web_search_preview tool so the
	// model can fetch real-time news to validate/invalidate the
	// alert thesis. Operator-billable (web_search calls have their
	// own cost line); default true now that the path is wired and
	// tested. Set false to fall back to Chat Completions.
	WebSearchEnabled      bool          `env:"AI_ANALYSIS_WEB_SEARCH_ENABLED" envDefault:"true"`
	WebContextMinSeverity string        `env:"AI_ANALYSIS_WEB_CONTEXT_MIN_SEVERITY" envDefault:"warning"`
	WebContextForHotInfo  bool          `env:"AI_ANALYSIS_WEB_CONTEXT_FOR_HOT_INFO" envDefault:"true"`
	WebContextForPolitics bool          `env:"AI_ANALYSIS_WEB_CONTEXT_FOR_POLITICS" envDefault:"true"`
	WebContextMaxResults  int           `env:"AI_ANALYSIS_WEB_CONTEXT_MAX_RESULTS" envDefault:"5" validate:"gte=1,lte=20"`
	WebContextTimeout     time.Duration `env:"AI_ANALYSIS_WEB_CONTEXT_TIMEOUT" envDefault:"12s" validate:"gt=0"`

	// Refresh policy.
	LifecycleRefreshDeltaPct float64 `env:"AI_ANALYSIS_LIFECYCLE_REFRESH_DELTA_PCT" envDefault:"1" validate:"gte=0"`
	CLVMaterialChange        float64 `env:"AI_ANALYSIS_CLV_MATERIAL_CHANGE" envDefault:"0.02" validate:"gte=0"`

	// --- v9.7 alias timeouts for downstream surfaces -----------------------
	// Per-surface timeouts. The alert + outcome paths are the only
	// AI surfaces left; the catalyst importer uses its own timeout
	// (configured under CatalystConfig).
	AlertAITimeout    time.Duration `env:"ALERT_AI_TIMEOUT" envDefault:"45s" validate:"gt=0"`
	CatalystAITimeout time.Duration `env:"CATALYST_AI_TIMEOUT" envDefault:"60s" validate:"gt=0"`
	OutcomeAITimeout  time.Duration `env:"OUTCOME_AI_TIMEOUT" envDefault:"45s" validate:"gt=0"`

	// --- v11.0 Hourly News Intelligence ---
	// One AI call per hour over NEW Polymarket annotations/news.
	// Replaces the killed prediction + market-intel surfaces.
	// AI is silent (sentinel) when no new news or nothing
	// actionable — operator gets messages ONLY when there is
	// real underpriced/repricing intelligence.
	NewsIntelEnabled           bool          `env:"NEWS_INTEL_ENABLED" envDefault:"true"`
	NewsIntelStartupRun        bool          `env:"NEWS_INTEL_STARTUP_RUN" envDefault:"true"`
	NewsIntelInterval          time.Duration `env:"NEWS_INTEL_INTERVAL" envDefault:"1h" validate:"gt=0"`
	NewsIntelLookback          time.Duration `env:"NEWS_INTEL_LOOKBACK" envDefault:"1h" validate:"gt=0"`
	NewsIntelMaxItems          int           `env:"NEWS_INTEL_MAX_ITEMS" envDefault:"100" validate:"gte=1,lte=500"`
	NewsIntelMaxMarketsPerItem int           `env:"NEWS_INTEL_MAX_MARKETS_PER_ITEM" envDefault:"5" validate:"gte=1,lte=20"`
	NewsIntelMaxSelected       int           `env:"NEWS_INTEL_MAX_SELECTED" envDefault:"8" validate:"gte=1,lte=50"`
	NewsIntelAIEnabled         bool          `env:"NEWS_INTEL_AI_ENABLED" envDefault:"true"`
	NewsIntelAITimeout         time.Duration `env:"NEWS_INTEL_AI_TIMEOUT" envDefault:"60s" validate:"gt=0"`
	NewsIntelSendTelegram      bool          `env:"NEWS_INTEL_SEND_TELEGRAM" envDefault:"true"`
	NewsIntelSuppressNoEdge    bool          `env:"NEWS_INTEL_SUPPRESS_NO_EDGE" envDefault:"true"`
	NewsIntelDedupeEnabled     bool          `env:"NEWS_INTEL_DEDUPE_ENABLED" envDefault:"true"`
	NewsIntelSemanticCooldown  time.Duration `env:"NEWS_INTEL_SEMANTIC_COOLDOWN" envDefault:"12h" validate:"gt=0"`
	NewsIntelMinConfidence     float64       `env:"NEWS_INTEL_MIN_CONFIDENCE" envDefault:"0.60" validate:"gte=0,lte=1"`

	// v11.0 hard lock — even if the legacy prediction-evolution
	// worker is somehow re-enabled, this final gate blocks the
	// "PREDICTION UPDATE · blocked" Telegram surface entirely.
	PredictionBlockedTelegramEnabled bool `env:"PREDICTION_BLOCKED_TELEGRAM_ENABLED" envDefault:"false"`

	// v11.1 — fine-grained Telegram surface kill switches, enforced
	// by the central telegram.Guard layer. Each flag is a hard "do
	// not deliver" gate that operates AT THE SENDER, independently
	// of whichever worker generated the body. Even if a worker is
	// accidentally re-enabled upstream, these flags suppress its
	// output and emit watchtower_telegram_suppressed_total{surface}.
	//
	// Defaults are FALSE — disabled. This is the production state
	// the operator wants after the v11.x cleanup.
	WatchtowerStatsTelegramEnabled           bool `env:"WATCHTOWER_STATS_TELEGRAM_ENABLED" envDefault:"false"`
	PredictionUpdateTelegramEnabled          bool `env:"PREDICTION_UPDATE_TELEGRAM_ENABLED" envDefault:"false"`
	PredictionStateTransitionTelegramEnabled bool `env:"PREDICTION_STATE_TRANSITION_TELEGRAM_ENABLED" envDefault:"false"`
}

// DetectionConfig tunes the v6 detection worker that drains
// polymarket_trades.detected_at IS NULL through detect.Loop.Observe.
// The worker only runs when Postgres is wired; memory-only dev mode
// keeps the v4 inline-from-collect behaviour.
type DetectionConfig struct {
	Workers    int           `env:"DETECTION_WORKERS" envDefault:"16" validate:"gte=1,lte=128"`
	ClaimLimit int           `env:"DETECTION_CLAIM_LIMIT" envDefault:"500" validate:"gte=1,lte=10000"`
	Interval   time.Duration `env:"DETECTION_INTERVAL" envDefault:"5s" validate:"gt=0"`
	// ClaimTTL is the lease duration on a claimed-but-not-yet-stamped
	// row. After this elapses a worker can reclaim the row (crash
	// recovery). Must exceed the per-row processing budget; default 5m.
	ClaimTTL time.Duration `env:"DETECTION_CLAIM_TTL" envDefault:"5m" validate:"gt=0"`
}

func LoadConfig() (*Config, error) {
	// v11.2: reject stale env keys at startup. Every surface listed
	// below was removed in v11.x and re-introducing it via env would
	// silently do nothing. Failing loud at boot tells the operator
	// "drop these from your environment" before we ship a release
	// that ignores them.
	if err := rejectStaleEnvKeys(); err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	if err := validator.New().Struct(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if err := cfg.validateInvariants(); err != nil {
		return nil, fmt.Errorf("validate config invariants: %w", err)
	}
	return cfg, nil
}

// staleEnvKeys names every env var the v11.2 cleanup retired. Listed
// keys are exact (no prefix matching) — operators that just want to
// disable a surface should remove the variable entirely; "=false"
// won't satisfy the guard. Each entry is one of:
//   - the variable that gated a deleted surface;
//   - a legacy alias for a current variable;
//   - a tuning knob whose backing field is gone.
var staleEnvKeys = []string{
	// v11.0 surfaces fully retired in v11.2.
	"AI_MARKET_INTELLIGENCE_ENABLED",
	"AI_MARKET_INTELLIGENCE_INTERVAL",
	"AI_MARKET_INTELLIGENCE_MAX_MARKETS",
	"AI_MARKET_INTELLIGENCE_MAX_OUTPUT_CHARS",
	"MARKET_INTEL_ENABLED",
	"MARKET_INTEL_LEGACY_ENABLED",
	"MARKET_INTEL_MIN_SEND_INTERVAL",
	"MARKET_INTEL_AI_TIMEOUT",
	"MARKET_INTEL_ANNOTATION_RANKING_AI_TIMEOUT",
	"MARKET_INTEL_RETRY_ON_TIMEOUT",
	"MARKET_INTEL_RETRY_BACKOFF_MIN",
	"MARKET_INTEL_RETRY_BACKOFF_MAX",
	"MARKET_INTEL_SOURCE_LINKS_ENABLED",
	"MARKET_INTEL_MAX_SOURCE_LINKS",
	"MARKET_INTEL_MAX_LINKS_PER_ROW",
	"MARKET_INTEL_FALLBACK_ON_AI_FAILURE",
	"MARKET_INTEL_SUPPRESS_ON_SENTINEL",
	"MARKET_INTEL_ANNOTATIONS_PER_EVENT",
	"MARKET_INTEL_VISIBLE_MARKETS",
	"MARKET_INTEL_DAILY_BUDGET_USD",
	"DAILY_POLITICAL_INTEL_ENABLED",
	"DAILY_POLITICAL_INTEL_TIME",
	"DAILY_POLITICAL_INTEL_TIMEZONE",
	"DAILY_POLITICAL_INTEL_MARKET_LIMIT",
	"DAILY_POLITICAL_INTEL_ANNOTATIONS_PER_MARKET",
	"DAILY_POLITICAL_INTEL_AI_ENABLED",
	"DAILY_POLITICAL_INTEL_AI_TIMEOUT",
	"DAILY_POLITICAL_INTEL_PROMPT_MAX_CHARS",
	"DAILY_POLITICAL_INTEL_SEND_TELEGRAM",
	"DAILY_INTEL_LEGACY_ENABLED",
	"DAILY_INTEL_DAILY_BUDGET_USD",
	"AI_MAX_INPUT_CHARS_DAILY_INTEL",
	"AI_MAX_INPUT_CHARS_MARKET_INTEL",
	"AI_MAX_OUTPUT_TOKENS_DAILY_INTEL",
	"ANNOTATION_RANKING_AI_ENABLED",
	"ANNOTATION_RANKING_DAILY_BUDGET_USD",
	"UNIFIED_INTEL_ENABLED",
	"UNIFIED_INTEL_MIN_QUERY_INTERVAL",
	"UNIFIED_INTEL_MIN_SEND_INTERVAL",
	// Prediction surfaces.
	"PREDICTION_CREATION_ENABLED",
	"PREDICTION_EVOLUTION_ENABLED",
	"PREDICTION_FEEDBACK_ENABLED",
	"PREDICTION_ARCHIVAL_ENABLED",
	"MARKET_PREDICTION_CREATION_ENABLED",
	"MARKET_PREDICTION_EVOLUTION_ENABLED",
	"MARKET_PREDICTION_CREATION_SEND_TELEGRAM",
	"MARKET_PREDICTION_EVOLUTION_SEND_TELEGRAM",
	"PREDICTION_CREATION_DAILY_BUDGET_USD",
	"PREDICTION_EVOLUTION_DAILY_BUDGET_USD",
	"PREDICTION_AI_LEGACY_ENABLED",
	"PREDICTION_CALIBRATION_REPORT_ENABLED",
	"PREDICTION_CREATION_AI_TIMEOUT",
	"PREDICTION_EVOLUTION_AI_TIMEOUT",
	"AI_MAX_INPUT_CHARS_PREDICTION_CREATE",
	"AI_MAX_INPUT_CHARS_PREDICTION_EVOLUTION",
	"AI_MAX_OUTPUT_TOKENS_PREDICTION",
	"AI_MAX_OUTPUT_TOKENS_EVOLUTION",
	"PREDICTION_BLOCKED_TELEGRAM_ENABLED",
	"PREDICTION_UPDATE_TELEGRAM_ENABLED",
	"PREDICTION_STATE_TRANSITION_TELEGRAM_ENABLED",
	// Signal-report Telegram scheduler.
	"SIGNAL_REPORTS_ENABLED",
	"SIGNAL_REPORTS_TIMEZONE",
	"SIGNAL_REPORTS_DAILY_AT",
	"SIGNAL_REPORTS_WEEKLY_AT",
	"SIGNAL_REPORTS_MONTHLY_AT",
	"SIGNAL_REPORTS_QUARTERLY_AT",
	"SIGNAL_REPORTS_YEARLY_AT",
	"SIGNAL_REPORTS_YEARLY_DELAY",
	"SIGNAL_REPORTS_TICK_INTERVAL",
	// Stable-favorite strategy.
	"STABLE_FAVORITE_ENABLED",
	"AI_ANALYSIS_WEB_CONTEXT_FOR_STABLE_FAVORITE",
	// Repricing module (only used by deleted prediction worker).
	"REPRICING_ENABLED",
	"REPRICING_LOOKBACK",
	"REPRICING_PRE_WINDOW",
	"REPRICING_POST_WINDOW",
	"REPRICING_MIN_ANNOTATION_MOVE",
	"REPRICING_MIN_FLOW_USD",
	"REPRICING_UNDERREACTION_THRESHOLD",
	"REPRICING_OVERREACTION_THRESHOLD",
	// Watchtower stats Telegram alias (the underlying
	// TELEGRAM_STATS_ENABLED switch is kept for the metrics path).
	"WATCHTOWER_STATS_TELEGRAM_ENABLED",
	// Pre-v11 narrative-context surfaces.
	"MARKET_ACTIVITY_CONTEXT_LOOKBACK",
	"EVENT_NARRATIVE_CONTEXT_LOOKBACK",
	// v11.12-insider-prior: WS_MAX_TOKENS retired. The old knob
	// silently truncated tokens after market selection, masking the
	// multi-outcome (May/Jul/Dec) failure mode. The replacement is
	// WS_MAX_TOKENS_HARD_CAP, which is a circuit-breaker only and
	// fails LOUDLY with ws.ErrTokenHardCapExceeded if breached.
	"WS_MAX_TOKENS",
}

func rejectStaleEnvKeys() error {
	for _, k := range staleEnvKeys {
		if _, present := os.LookupEnv(k); present {
			return fmt.Errorf("unsupported legacy env key %s; this surface was removed in the v11.x cleanup. Remove it from the environment.", k)
		}
	}
	return nil
}

// validateInvariants applies cross-field rules the struct-tag
// validator can't express. v10.6 added the WS subscription safety
// belts here so an accidental WS_MAX_MARKETS=2500 fails fast at
// boot instead of fanning out a 5000-token subscription.
func (c *Config) validateInvariants() error {
	const wsHardCap = 250
	if c.WS.Enabled {
		if c.WS.MaxMarkets <= 0 {
			return fmt.Errorf("WS_ENABLED=true requires WS_MAX_MARKETS > 0 (got %d)", c.WS.MaxMarkets)
		}
		// v11.12-insider-prior: WS_MAX_TOKENS_HARD_CAP is a circuit-
		// breaker only; it MUST be far above any realistic
		// market-count × tokens-per-market product. Refuse boot if
		// the operator pins it below WS_MAX_MARKETS — that would
		// guarantee a hard-cap trip on a healthy single-outcome
		// subscription.
		if c.WS.MaxTokensHardCap < c.WS.MaxMarkets {
			return fmt.Errorf("WS_MAX_TOKENS_HARD_CAP (%d) must be >= WS_MAX_MARKETS (%d) — the cap is a circuit-breaker, not a tuning knob",
				c.WS.MaxTokensHardCap, c.WS.MaxMarkets)
		}
		if c.WS.MaxMarkets > wsHardCap && !c.WS.AllowLargeSubscription {
			return fmt.Errorf("WS_MAX_MARKETS=%d exceeds the safety cap of %d; "+
				"set WS_ALLOW_LARGE_SUBSCRIPTION=true to override",
				c.WS.MaxMarkets, wsHardCap)
		}
	}
	if err := c.Strategy.validateInvariants(); err != nil {
		return err
	}
	return nil
}
