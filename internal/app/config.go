package app

import (
	"fmt"
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

// AlertingConfig wires sinks. The Telegram sink sends to a single configured
// chat — there is no subscriber discovery, no /getUpdates polling, no
// dynamic broadcast. Set TELEGRAM_CHAT_ID once and that's the only chat
// that will ever receive alerts.
type AlertingConfig struct {
	WebhookURL       string        `env:"ALERT_WEBHOOK_URL"`
	TelegramEnabled  bool          `env:"TELEGRAM_ENABLED" envDefault:"false"`
	TelegramBotToken string        `env:"TELEGRAM_BOT_TOKEN"`
	TelegramChatID   string        `env:"TELEGRAM_CHAT_ID"`
	TelegramBaseURL  string        `env:"TELEGRAM_BASE_URL"`
	TelegramTimeout  time.Duration `env:"TELEGRAM_TIMEOUT" envDefault:"5s"`

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

// SignalReportConfig wires the scheduled signal-quality reports
// (daily / weekly / monthly / quarterly / yearly). Decisions:
//
//   - Timezone is operator-controlled. Etc/GMT-3 = UTC+3 in the IANA
//     database (the sign is inverted by historical convention).
//   - Send time is per-period because the daily report at 08:00 and a
//     yearly report at 09:00 might be reasonable for a different team.
//     Defaults are all 08:00 to match the spec.
//   - YearlyDelay (72h default) gives late upstream resolution one
//     business cycle to settle before the year-end report locks in.
type SignalReportConfig struct {
	Enabled      bool          `env:"SIGNAL_REPORTS_ENABLED" envDefault:"false"`
	Timezone     string        `env:"SIGNAL_REPORTS_TIMEZONE" envDefault:"Etc/GMT-3"`
	DailyAt      string        `env:"SIGNAL_REPORTS_DAILY_AT" envDefault:"08:00"`
	WeeklyAt     string        `env:"SIGNAL_REPORTS_WEEKLY_AT" envDefault:"08:00"`
	MonthlyAt    string        `env:"SIGNAL_REPORTS_MONTHLY_AT" envDefault:"08:00"`
	QuarterlyAt  string        `env:"SIGNAL_REPORTS_QUARTERLY_AT" envDefault:"08:00"`
	YearlyAt     string        `env:"SIGNAL_REPORTS_YEARLY_AT" envDefault:"08:00"`
	YearlyDelay  time.Duration `env:"SIGNAL_REPORTS_YEARLY_DELAY" envDefault:"72h"`
	TickInterval time.Duration `env:"SIGNAL_REPORTS_TICK_INTERVAL" envDefault:"1m"`
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
	SignalReport      SignalReportConfig
	TelegramReactions TelegramReactionsConfig
	Polymarket        PolymarketConfig
	RateLimit         RateLimitConfig
	Pipeline          PipelineConfig
	Anomaly           AnomalyConfig
	CategoryFilter    CategoryFilterConfig
	Alerting          AlertingConfig
	StatsReport       StatsReportConfig
	Detection         DetectionConfig
	StableFavorite    StableFavoriteConfig
	AIAnalysis        AIAnalysisConfig
	EventPage         EventPageContextConfig
	Catalyst          CatalystConfig
	DailyIntel        DailyPoliticalIntelConfig
	EventFlow         EventFlowConfig
	Repricing         RepricingConfig
	Prediction        PredictionConfig
	AIBudget          AIBudgetConfig
	AIPreflight       AIPreflightConfig
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

// RepricingConfig drives the deterministic per-annotation repricing
// signal. AI consumes the signal as evidence; the layer itself is
// pure math.
type RepricingConfig struct {
	Enabled                bool          `env:"REPRICING_INTELLIGENCE_ENABLED" envDefault:"true"`
	Lookback               time.Duration `env:"REPRICING_LOOKBACK" envDefault:"24h" validate:"gt=0"`
	PreWindow              time.Duration `env:"REPRICING_PRE_WINDOW" envDefault:"2h" validate:"gt=0"`
	PostWindow             time.Duration `env:"REPRICING_POST_WINDOW" envDefault:"2h" validate:"gt=0"`
	MinAnnotationMove      float64       `env:"REPRICING_MIN_ANNOTATION_MOVE" envDefault:"0.05" validate:"gt=0,lte=1"`
	MinFlowUSD             float64       `env:"REPRICING_MIN_FLOW_USD" envDefault:"5000" validate:"gte=0"`
	UnderreactionThreshold float64       `env:"REPRICING_UNDERREACTION_THRESHOLD" envDefault:"0.03" validate:"gt=0,lte=1"`
	OverreactionThreshold  float64       `env:"REPRICING_OVERREACTION_THRESHOLD" envDefault:"0.08" validate:"gt=0,lte=1"`
}

// PredictionConfig drives the prediction state machine + evolution
// worker.
type PredictionConfig struct {
	StateEnabled            bool          `env:"MARKET_PREDICTION_STATE_ENABLED" envDefault:"true"`
	StaleAfter              time.Duration `env:"MARKET_PREDICTION_STALE_AFTER" envDefault:"24h" validate:"gt=0"`
	ConfirmAlertScoreFloor  float64       `env:"MARKET_PREDICTION_CONFIRM_ALERT_SCORE" envDefault:"0.60" validate:"gte=0,lte=1"`
	ContradictFlowImbalance float64       `env:"MARKET_PREDICTION_CONTRADICT_FLOW_IMBALANCE" envDefault:"0.65" validate:"gte=0,lte=1"`

	// v9.9 Evolution worker (heartbeat).
	EvolutionEnabled     bool          `env:"MARKET_PREDICTION_EVOLUTION_ENABLED" envDefault:"true"`
	EvolutionInterval    time.Duration `env:"MARKET_PREDICTION_EVOLUTION_INTERVAL" envDefault:"15m" validate:"gt=0"`
	EvolutionBatchSize   int           `env:"MARKET_PREDICTION_EVOLUTION_BATCH_SIZE" envDefault:"100" validate:"gte=1,lte=1000"`
	EvolutionConcurrency int           `env:"MARKET_PREDICTION_EVOLUTION_CONCURRENCY" envDefault:"4" validate:"gte=1,lte=32"`
	EvolutionTimeout     time.Duration `env:"MARKET_PREDICTION_EVOLUTION_TIMEOUT" envDefault:"60s" validate:"gt=0"`

	EvolutionAIEnabled     bool          `env:"MARKET_PREDICTION_EVOLUTION_AI_ENABLED" envDefault:"true"`
	EvolutionAIMinInterval time.Duration `env:"MARKET_PREDICTION_EVOLUTION_AI_MIN_INTERVAL" envDefault:"6h" validate:"gt=0"`
	EvolutionAIMaxPerRun   int           `env:"MARKET_PREDICTION_EVOLUTION_AI_MAX_PER_RUN" envDefault:"10" validate:"gte=0,lte=200"`

	EvolutionStaleAfter    time.Duration `env:"MARKET_PREDICTION_EVOLUTION_STALE_AFTER" envDefault:"24h" validate:"gt=0"`
	EvolutionDecayEnabled  bool          `env:"MARKET_PREDICTION_EVOLUTION_DECAY_ENABLED" envDefault:"true"`
	EvolutionDecayPerDay   float64       `env:"MARKET_PREDICTION_EVOLUTION_DECAY_PER_DAY" envDefault:"0.15" validate:"gte=0,lte=1"`
	EvolutionMinConfidence float64       `env:"MARKET_PREDICTION_EVOLUTION_MIN_CONFIDENCE" envDefault:"0.10" validate:"gte=0,lte=1"`

	EvolutionMajorPriceMove     float64       `env:"MARKET_PREDICTION_EVOLUTION_MAJOR_PRICE_MOVE" envDefault:"0.08" validate:"gte=0,lte=1"`
	EvolutionCatalystNearWindow time.Duration `env:"MARKET_PREDICTION_EVOLUTION_CATALYST_NEAR_WINDOW" envDefault:"12h" validate:"gt=0"`

	EvolutionSendTelegram     bool          `env:"MARKET_PREDICTION_EVOLUTION_SEND_TELEGRAM" envDefault:"true"`
	EvolutionTelegramCooldown time.Duration `env:"MARKET_PREDICTION_EVOLUTION_TELEGRAM_COOLDOWN" envDefault:"6h" validate:"gt=0"`

	// --- v10.0 prediction creation worker (PART 1) ---
	// The cold-start path. Without this loop, the evolution worker
	// has nothing to evolve. Defaults are moderate / safe / non-
	// spammy — the deterministic shortlist + AI ranking step keeps
	// the AI bill bounded under the AI budget governor.
	CreationEnabled      bool          `env:"MARKET_PREDICTION_CREATION_ENABLED" envDefault:"true"`
	CreationInterval     time.Duration `env:"MARKET_PREDICTION_CREATION_INTERVAL" envDefault:"30m" validate:"gt=0"`
	CreationBatchSize    int           `env:"MARKET_PREDICTION_CREATION_BATCH_SIZE" envDefault:"150" validate:"gte=10,lte=1000"`
	CreationMaxSelected  int           `env:"MARKET_PREDICTION_CREATION_MAX_SELECTED" envDefault:"10" validate:"gte=1,lte=50"`
	CreationMinScore     float64       `env:"MARKET_PREDICTION_CREATION_MIN_SCORE" envDefault:"0.55" validate:"gte=0,lte=1"`
	CreationMaxPerDay    int           `env:"MARKET_PREDICTION_CREATION_MAX_PER_DAY" envDefault:"40" validate:"gte=1,lte=500"`
	CreationDedupeWindow time.Duration `env:"MARKET_PREDICTION_CREATION_DEDUPE_WINDOW" envDefault:"24h" validate:"gt=0"`
	CreationAIEnabled    bool          `env:"MARKET_PREDICTION_CREATION_AI_ENABLED" envDefault:"true"`
	CreationAIModel      string        `env:"MARKET_PREDICTION_CREATION_AI_MODEL" envDefault:"gpt-4.1"`
	CreationAITimeout    time.Duration `env:"MARKET_PREDICTION_CREATION_AI_TIMEOUT" envDefault:"60s" validate:"gt=0"`
	CreationConcurrency  int           `env:"MARKET_PREDICTION_CREATION_CONCURRENCY" envDefault:"2" validate:"gte=1,lte=16"`
	CreationSendTelegram bool          `env:"MARKET_PREDICTION_CREATION_SEND_TELEGRAM" envDefault:"true"`
	CreationCategories   []string      `env:"MARKET_PREDICTION_CREATION_CATEGORIES" envDefault:"politics,geopolitics,elections" envSeparator:","`

	// --- v10.1 Telegram polish (PART 1/3/5/7) ---------------------
	// Annotations + Links blocks under the AI thesis.
	TelegramAnnotationsEnabled        bool `env:"MARKET_PREDICTION_TELEGRAM_ANNOTATIONS_ENABLED" envDefault:"true"`
	TelegramAnnotationsLimit          int  `env:"MARKET_PREDICTION_TELEGRAM_ANNOTATIONS_LIMIT" envDefault:"5" validate:"gte=0,lte=20"`
	TelegramAnnotationsMaxTitleChars  int  `env:"MARKET_PREDICTION_TELEGRAM_ANNOTATIONS_MAX_TITLE_CHARS" envDefault:"160" validate:"gte=20,lte=512"`
	TelegramAnnotationsMaxSourceNames int  `env:"MARKET_PREDICTION_TELEGRAM_ANNOTATIONS_MAX_SOURCE_NAMES" envDefault:"3" validate:"gte=0,lte=10"`
	TelegramLinksEnabled              bool `env:"MARKET_PREDICTION_TELEGRAM_LINKS_ENABLED" envDefault:"true"`

	// Per-event Telegram cooldown for the creation worker (PART 5).
	// In-memory map + a deterministic skip-reason log line.
	CreationTelegramCooldown  time.Duration `env:"MARKET_PREDICTION_CREATION_TELEGRAM_COOLDOWN" envDefault:"6h" validate:"gt=0"`
	CreationMaxTelegramPerRun int           `env:"MARKET_PREDICTION_CREATION_MAX_TELEGRAM_PER_RUN" envDefault:"3" validate:"gte=0,lte=50"`
	CreationSendOnStartup     bool          `env:"MARKET_PREDICTION_CREATION_SEND_ON_STARTUP" envDefault:"false"`

	// Quality gate (PART 7). Persist always (if PersistLowQuality);
	// gate Telegram send strictly.
	CreationSendNeutral       bool    `env:"MARKET_PREDICTION_CREATION_SEND_NEUTRAL" envDefault:"false"`
	CreationPersistLowQuality bool    `env:"MARKET_PREDICTION_CREATION_PERSIST_LOW_QUALITY" envDefault:"true"`
	CreationMinConfidence     float64 `env:"MARKET_PREDICTION_CREATION_MIN_CONFIDENCE" envDefault:"0.55" validate:"gte=0,lte=1"`
	CreationRequireSignal     bool    `env:"MARKET_PREDICTION_CREATION_REQUIRE_SIGNAL" envDefault:"true"`
	CreationMinSummaryChars   int     `env:"MARKET_PREDICTION_CREATION_MIN_SUMMARY_CHARS" envDefault:"300" validate:"gte=0,lte=10000"`

	// --- v10.2 usefulness scoring (PART 3) ------------------------
	UsefulnessEnabled      bool    `env:"PREDICTION_USEFULNESS_ENABLED" envDefault:"true"`
	UsefulnessMinTelegram  float64 `env:"PREDICTION_USEFULNESS_MIN_TELEGRAM_SCORE" envDefault:"0.60" validate:"gte=0,lte=1"`
	UsefulnessHighPriority float64 `env:"PREDICTION_USEFULNESS_HIGH_PRIORITY_SCORE" envDefault:"0.80" validate:"gte=0,lte=1"`

	// --- v10.2 feedback worker (PART 4) ---------------------------
	FeedbackEnabled     bool          `env:"PREDICTION_FEEDBACK_ENABLED" envDefault:"true"`
	FeedbackInterval    time.Duration `env:"PREDICTION_FEEDBACK_INTERVAL" envDefault:"15m" validate:"gt=0"`
	FeedbackHorizonsCSV string        `env:"PREDICTION_FEEDBACK_HORIZONS" envDefault:"1h,6h,24h"`
	FeedbackBatchSize   int           `env:"PREDICTION_FEEDBACK_BATCH_SIZE" envDefault:"100" validate:"gte=1,lte=1000"`

	// --- v10.3 evaluation classifier knobs (PART 2) -------------
	EvaluationEnabled           bool    `env:"PREDICTION_EVALUATION_ENABLED" envDefault:"true"`
	EvaluationHorizonsCSV       string  `env:"PREDICTION_EVALUATION_HORIZONS" envDefault:"1h,6h,24h"`
	EvaluationMinPriceDelta     float64 `env:"PREDICTION_EVALUATION_MIN_PRICE_DELTA" envDefault:"0.03" validate:"gte=0,lte=1"`
	EvaluationUsefulEarlyWindow string  `env:"PREDICTION_EVALUATION_USEFUL_EARLY_WINDOW" envDefault:"6h"`

	// --- v10.3 archival worker knobs (PART 4) -------------------
	ArchivalEnabled            bool          `env:"PREDICTION_ARCHIVAL_ENABLED" envDefault:"true"`
	ArchivalInterval           time.Duration `env:"PREDICTION_ARCHIVAL_INTERVAL" envDefault:"1h" validate:"gt=0"`
	ArchivalTerminalRetention  time.Duration `env:"PREDICTION_ARCHIVAL_TERMINAL_RETENTION" envDefault:"72h" validate:"gt=0"`
	ArchivalStaleNoSignalAfter time.Duration `env:"PREDICTION_STALE_NO_SIGNAL_AFTER" envDefault:"18h" validate:"gt=0"`
	ArchivalBlockedRevalidate  time.Duration `env:"PREDICTION_BLOCKED_REVALIDATE_INTERVAL" envDefault:"6h" validate:"gt=0"`
	ArchivalBatchSize          int           `env:"PREDICTION_ARCHIVAL_BATCH_SIZE" envDefault:"200" validate:"gte=1,lte=2000"`

	// --- v10.3 calibration report (PART 7) ----------------------
	CalibrationReportEnabled      bool   `env:"PREDICTION_CALIBRATION_REPORT_ENABLED" envDefault:"true"`
	CalibrationReportTime         string `env:"PREDICTION_CALIBRATION_REPORT_TIME" envDefault:"09:00"`
	CalibrationReportSendTelegram bool   `env:"PREDICTION_CALIBRATION_REPORT_SEND_TELEGRAM" envDefault:"true"`
}

// AIPreflightConfig wires the v10.3 per-surface preflight caps.
// Every AI surface routes its prompt through aipreflight.Preflight
// which enforces these caps + the AIBudgetConfig daily budgets.
type AIPreflightConfig struct {
	MaxInputCharsAlert              int `env:"AI_MAX_INPUT_CHARS_ALERT" envDefault:"18000" validate:"gte=1000"`
	MaxInputCharsCatalyst           int `env:"AI_MAX_INPUT_CHARS_CATALYST" envDefault:"18000" validate:"gte=1000"`
	MaxInputCharsPredictionCreate   int `env:"AI_MAX_INPUT_CHARS_PREDICTION_CREATE" envDefault:"22000" validate:"gte=1000"`
	MaxInputCharsPredictionEvolve   int `env:"AI_MAX_INPUT_CHARS_PREDICTION_EVOLUTION" envDefault:"18000" validate:"gte=1000"`
	MaxInputCharsDailyIntel         int `env:"AI_MAX_INPUT_CHARS_DAILY_INTEL" envDefault:"35000" validate:"gte=1000"`
	MaxInputCharsMarketIntel        int `env:"AI_MAX_INPUT_CHARS_MARKET_INTEL" envDefault:"20000" validate:"gte=1000"`
	MaxOutputTokensAlert            int `env:"AI_MAX_OUTPUT_TOKENS_ALERT" envDefault:"1200" validate:"gte=200"`
	MaxOutputTokensPredictionCreate int `env:"AI_MAX_OUTPUT_TOKENS_PREDICTION" envDefault:"1200" validate:"gte=200"`
	MaxOutputTokensPredictionEvolve int `env:"AI_MAX_OUTPUT_TOKENS_EVOLUTION" envDefault:"1000" validate:"gte=200"`
	MaxOutputTokensCatalyst         int `env:"AI_MAX_OUTPUT_TOKENS_CATALYST" envDefault:"1200" validate:"gte=200"`
	MaxOutputTokensDailyIntel       int `env:"AI_MAX_OUTPUT_TOKENS_DAILY_INTEL" envDefault:"2500" validate:"gte=200"`
}

// AIBudgetConfig wires the process-local AI budget governor (PART 5
// of the v10.0 operational pass). 0 on any field disables that
// specific cap; the recommended production values are the defaults
// below and live in CLAUDE.md / .env.example.
type AIBudgetConfig struct {
	GlobalDailyUSD             float64 `env:"AI_GLOBAL_DAILY_BUDGET_USD" envDefault:"25" validate:"gte=0"`
	AlertAnalysisDailyUSD      float64 `env:"AI_ANALYSIS_DAILY_BUDGET_USD_OVERRIDE" envDefault:"0" validate:"gte=0"`
	CatalystImporterDailyUSD   float64 `env:"EVENT_CATALYST_IMPORTER_DAILY_BUDGET_USD" envDefault:"8" validate:"gte=0"`
	PredictionCreationDailyUSD float64 `env:"PREDICTION_CREATION_DAILY_BUDGET_USD" envDefault:"8" validate:"gte=0"`
	PredictionEvolveDailyUSD   float64 `env:"PREDICTION_EVOLUTION_DAILY_BUDGET_USD" envDefault:"5" validate:"gte=0"`
	MarketIntelDailyUSD        float64 `env:"MARKET_INTEL_DAILY_BUDGET_USD" envDefault:"2" validate:"gte=0"`
	DailyIntelDailyUSD         float64 `env:"DAILY_INTEL_DAILY_BUDGET_USD" envDefault:"2" validate:"gte=0"`
	AnnotationRankDailyUSD     float64 `env:"ANNOTATION_RANKING_DAILY_BUDGET_USD" envDefault:"2" validate:"gte=0"`
}

// DailyPoliticalIntelConfig wires the v9.7 once-per-day political /
// geopolitical intelligence report. Failure NEVER blocks alerts;
// the worker is fully decoupled from the alert pipeline.
type DailyPoliticalIntelConfig struct {
	Enabled              bool          `env:"DAILY_POLITICAL_INTEL_ENABLED" envDefault:"true"`
	TimeOfDay            string        `env:"DAILY_POLITICAL_INTEL_TIME" envDefault:"08:00"`
	Timezone             string        `env:"DAILY_POLITICAL_INTEL_TIMEZONE" envDefault:"Europe/Tallinn"`
	MarketLimit          int           `env:"DAILY_POLITICAL_INTEL_MARKET_LIMIT" envDefault:"100" validate:"gte=10,lte=500"`
	AnnotationsPerMarket int           `env:"DAILY_POLITICAL_INTEL_ANNOTATIONS_PER_MARKET" envDefault:"4" validate:"gte=1,lte=20"`
	AIEnabled            bool          `env:"DAILY_POLITICAL_INTEL_AI_ENABLED" envDefault:"true"`
	AITimeout            time.Duration `env:"DAILY_POLITICAL_INTEL_AI_TIMEOUT" envDefault:"90s" validate:"gt=0"`
	PromptMaxChars       int           `env:"DAILY_POLITICAL_INTEL_PROMPT_MAX_CHARS" envDefault:"30000" validate:"gte=2000,lte=80000"`
	SendTelegram         bool          `env:"DAILY_POLITICAL_INTEL_SEND_TELEGRAM" envDefault:"true"`
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

	Timeout        time.Duration `env:"AI_ANALYSIS_TIMEOUT" envDefault:"8s" validate:"gt=0"`
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
	WebSearchEnabled            bool          `env:"AI_ANALYSIS_WEB_SEARCH_ENABLED" envDefault:"true"`
	WebContextMinSeverity       string        `env:"AI_ANALYSIS_WEB_CONTEXT_MIN_SEVERITY" envDefault:"warning"`
	WebContextForHotInfo        bool          `env:"AI_ANALYSIS_WEB_CONTEXT_FOR_HOT_INFO" envDefault:"true"`
	WebContextForStableFavorite bool          `env:"AI_ANALYSIS_WEB_CONTEXT_FOR_STABLE_FAVORITE" envDefault:"true"`
	WebContextForPolitics       bool          `env:"AI_ANALYSIS_WEB_CONTEXT_FOR_POLITICS" envDefault:"true"`
	WebContextMaxResults        int           `env:"AI_ANALYSIS_WEB_CONTEXT_MAX_RESULTS" envDefault:"5" validate:"gte=1,lte=20"`
	WebContextTimeout           time.Duration `env:"AI_ANALYSIS_WEB_CONTEXT_TIMEOUT" envDefault:"12s" validate:"gt=0"`

	// Refresh policy.
	LifecycleRefreshDeltaPct float64 `env:"AI_ANALYSIS_LIFECYCLE_REFRESH_DELTA_PCT" envDefault:"1" validate:"gte=0"`
	CLVMaterialChange        float64 `env:"AI_ANALYSIS_CLV_MATERIAL_CHANGE" envDefault:"0.02" validate:"gte=0"`

	// 2h market-intelligence schedule.
	MarketIntelligenceEnabled        bool          `env:"AI_MARKET_INTELLIGENCE_ENABLED" envDefault:"false"`
	MarketIntelligenceInterval       time.Duration `env:"AI_MARKET_INTELLIGENCE_INTERVAL" envDefault:"2h" validate:"gt=0"`
	MarketIntelligenceMaxMarkets     int           `env:"AI_MARKET_INTELLIGENCE_MAX_MARKETS" envDefault:"50" validate:"gte=1,lte=500"`
	MarketIntelligenceMaxOutputChars int           `env:"AI_MARKET_INTELLIGENCE_MAX_OUTPUT_CHARS" envDefault:"2000" validate:"gte=200,lte=8000"`
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

// StableFavoriteConfig configures the late-market-stable-favorite
// strategy. SEPARATE from whale-flow knobs — must be toggled
// independently. The strategy looks for late-stage markets with a
// stable favorite in a defined probability band; it never represents
// itself as risk-free or guaranteed.
type StableFavoriteConfig struct {
	Enabled bool `env:"STABLE_FAVORITE_ENABLED" envDefault:"false"`

	// Lifecycle gates.
	MinLifecyclePct float64 `env:"STABLE_FAVORITE_MIN_LIFECYCLE_PCT" envDefault:"92" validate:"gte=0,lte=100"`
	HotLifecyclePct float64 `env:"STABLE_FAVORITE_HOT_LIFECYCLE_PCT" envDefault:"97" validate:"gte=0,lte=100"`

	// Favorite-probability band — we want neither a coinflip nor a
	// near-certain side (no payout).
	MinProbability float64 `env:"STABLE_FAVORITE_MIN_PROBABILITY" envDefault:"0.55" validate:"gt=0,lt=1"`
	MaxProbability float64 `env:"STABLE_FAVORITE_MAX_PROBABILITY" envDefault:"0.85" validate:"gt=0,lt=1"`

	// Remaining-return floor expressed as a percentage.
	MinReturnPct float64 `env:"STABLE_FAVORITE_MIN_RETURN_PCT" envDefault:"20" validate:"gte=0"`

	// Stability window. v7 relaxation: shortened to 6h (was 24h) so
	// the stability read reflects the most-recent regime, and the
	// stddev / drawdown / adverse-move caps loosened to admit markets
	// that breathe a little without flapping. Cross-market is NOT a
	// hard gate (see pickSeverity in the detector).
	StabilityWindow    time.Duration `env:"STABLE_FAVORITE_STABILITY_WINDOW" envDefault:"6h" validate:"gt=0"`
	MaxPriceStddev     float64       `env:"STABLE_FAVORITE_MAX_PRICE_STDDEV" envDefault:"0.10" validate:"gt=0"`
	MaxDrawdown        float64       `env:"STABLE_FAVORITE_MAX_DRAWDOWN" envDefault:"0.25" validate:"gt=0"`
	MaxAdverseMove6h   float64       `env:"STABLE_FAVORITE_MAX_ADVERSE_MOVE_6H" envDefault:"0.15" validate:"gt=0"`
	MaxNegativeDrift6h float64       `env:"STABLE_FAVORITE_MAX_NEGATIVE_DRIFT_6H" envDefault:"0.10" validate:"gte=0"`

	// Liquidity gates.
	MinMarketVolumeUSD float64 `env:"STABLE_FAVORITE_MIN_MARKET_VOLUME_USD" envDefault:"25000" validate:"gte=0"`
	MinRecentTrades    int     `env:"STABLE_FAVORITE_MIN_RECENT_TRADES" envDefault:"20" validate:"gte=0"`

	// Cross-market (optional; no upstream wired in v6). Effect is
	// confidence-only — see detector.pickSeverity.
	CrossMarketEnabled         bool    `env:"STABLE_FAVORITE_CROSS_MARKET_ENABLED" envDefault:"true"`
	MaxCrossMarketDisagreement float64 `env:"STABLE_FAVORITE_MAX_CROSS_MARKET_DISAGREEMENT" envDefault:"0.15" validate:"gte=0,lt=1"`

	// Worker cadence. 15m (was 5m) tracks the slower-moving state
	// view appropriate for the relaxed stability window.
	Interval       time.Duration `env:"STABLE_FAVORITE_INTERVAL" envDefault:"15m" validate:"gt=0"`
	CandidateLimit int           `env:"STABLE_FAVORITE_CANDIDATE_LIMIT" envDefault:"200" validate:"gte=1,lte=10000"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	if err := validator.New().Struct(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}
