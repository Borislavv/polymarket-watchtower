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
	// Single-trade severity ladders. Both ladders must qualify at the same
	// rung or higher; final severity is the lower of the two.
	InfoMinNotionalUSD     float64 `env:"ALERT_INFO_MIN_NOTIONAL_USD" envDefault:"10000" validate:"gte=0"`
	InfoMinOdds            float64 `env:"ALERT_INFO_MIN_ODDS" envDefault:"3" validate:"gte=1"`
	InfoMinMultiplier      float64 `env:"ALERT_INFO_MIN_MULTIPLIER" envDefault:"100" validate:"gte=0"`
	WarningMinNotionalUSD  float64 `env:"ALERT_WARNING_MIN_NOTIONAL_USD" envDefault:"25000" validate:"gte=0"`
	WarningMinOdds         float64 `env:"ALERT_WARNING_MIN_ODDS" envDefault:"5" validate:"gte=1"`
	WarningMinMultiplier   float64 `env:"ALERT_WARNING_MIN_MULTIPLIER" envDefault:"1000" validate:"gte=0"`
	CriticalMinNotionalUSD float64 `env:"ALERT_CRITICAL_MIN_NOTIONAL_USD" envDefault:"100000" validate:"gte=0"`
	CriticalMinOdds        float64 `env:"ALERT_CRITICAL_MIN_ODDS" envDefault:"8" validate:"gte=1"`
	CriticalMinMultiplier  float64 `env:"ALERT_CRITICAL_MIN_MULTIPLIER" envDefault:"10000" validate:"gte=0"`

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
