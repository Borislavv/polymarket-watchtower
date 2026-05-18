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

// AnomalyMode selects which detector wires alerts. single_cluster is the
// default; volume keeps the legacy aggregate-rate behaviour for operators
// who explicitly want it.
type AnomalyMode string

const (
	ModeSingleCluster AnomalyMode = "single_cluster"
	ModeVolume        AnomalyMode = "volume"
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
type BackfillConfig struct {
	// Interval is the tick cadence; each tick claims up to BatchSize
	// markets and runs a full backfill pass per market.
	Interval time.Duration `env:"BACKFILL_INTERVAL" envDefault:"1m" validate:"required"`
	// BatchSize is the max number of markets claimed per tick. Lower it
	// to reduce upstream pressure; higher to speed bootstrap.
	BatchSize int `env:"BACKFILL_WORKERS" envDefault:"4" validate:"gte=1"`
	// Concurrency caps in-flight backfills inside one tick.
	Concurrency int `env:"BACKFILL_CONCURRENCY" envDefault:"2" validate:"gte=1"`
	// PageLimit is the Data API page size (max 500).
	PageLimit int `env:"BACKFILL_PAGE_LIMIT" envDefault:"500" validate:"gte=1,lte=500"`
	// StaleAfter requeues 'running' markets older than this — used to
	// recover from a crashed previous process.
	StaleAfter time.Duration `env:"BACKFILL_STALE_AFTER" envDefault:"15m" validate:"required"`
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
type PipelineConfig struct {
	DiscoverInterval   time.Duration `env:"DISCOVER_INTERVAL" envDefault:"10m" validate:"required"`
	CollectInterval    time.Duration `env:"COLLECT_INTERVAL" envDefault:"60s" validate:"required"`
	MaxMarkets         int           `env:"MAX_MARKETS" envDefault:"500" validate:"gte=0"`
	ActiveOnly         bool          `env:"ACTIVE_ONLY" envDefault:"true"`
	OrderBy            string        `env:"DISCOVER_ORDER" envDefault:"volume_24hr"`
	CollectConcurrency int           `env:"COLLECT_CONCURRENCY" envDefault:"8" validate:"gte=1"`
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

// AggregateConfig sizes the rolling-bucket engine used by both modes
// (supporting gauges in single_cluster; alert source in volume).
type AggregateConfig struct {
	BucketSize     time.Duration   `env:"AGG_BUCKET" envDefault:"1m" validate:"required"`
	BaselineWindow time.Duration   `env:"AGG_BASELINE_WINDOW" envDefault:"168h" validate:"required"`
	RecentWindows  []time.Duration `env:"AGG_RECENT_WINDOWS" envDefault:"12h,24h" envSeparator:","`
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
type AnomalyConfig struct {
	Mode AnomalyMode `env:"ANOMALY_MODE" envDefault:"single_cluster" validate:"required,oneof=single_cluster volume"`

	// StrategyVersion is stamped on every persisted alert row and woven
	// into the dedup_key so a config retune cannot resurrect alerts
	// dropped under the previous strategy. v4 adds same-trader
	// accumulation-line detection on top of v2's trader-history
	// multiplier + MM/arbitrage suppression. Bump this when changing
	// tier thresholds, baseline gates, or any other decision input.
	StrategyVersion string `env:"STRATEGY_VERSION" envDefault:"v4" validate:"required"`

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
	BaselineWindow     time.Duration `env:"BASELINE_WINDOW" envDefault:"8760h" validate:"gte=0"`
	BaselineMaxSamples int           `env:"BASELINE_MAX_SAMPLES" envDefault:"1024" validate:"gte=16"`
	// BaselineMinReadySpan requires the observed baseline span (newest minus
	// oldest sample) to clear this floor before alerts can fire. Distinct
	// from BaselineWindow which is a *cap*. 0 disables.
	BaselineMinReadySpan time.Duration `env:"BASELINE_MIN_READY_WINDOW" envDefault:"24h" validate:"gte=0"`

	// Lifecycle gating: only alert when the market is in the last
	// (100 - LifecycleAlertFromPct)% of its lifetime. Markets with missing
	// start/end dates are silenced by default (fail-closed); set
	// ALLOW_UNKNOWN_MARKET_LIFECYCLE=true to opt in.
	LifecycleAlertFromPct       float64       `env:"LIFECYCLE_ALERT_FROM_PCT" envDefault:"75" validate:"gte=0,lte=100"`
	LifecycleHotFromPct         float64       `env:"LIFECYCLE_HOT_FROM_PCT" envDefault:"90" validate:"gte=0,lte=100"`
	MarketMinAge                time.Duration `env:"MARKET_MIN_AGE" envDefault:"24h" validate:"gte=0"`
	AllowUnknownMarketLifecycle bool          `env:"ALLOW_UNKNOWN_MARKET_LIFECYCLE" envDefault:"false"`

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

	// Cluster (HARD) alert. Fires when several already-firing single-trade
	// alerts converge on one category within ClusterWindow.
	ClusterWindow      time.Duration `env:"CLUSTER_WINDOW" envDefault:"30m" validate:"required"`
	ClusterMinTrades   int           `env:"CLUSTER_MIN_ANOMALOUS_TRADES" envDefault:"3" validate:"gte=2"`
	ClusterMinWallets  int           `env:"CLUSTER_MIN_UNIQUE_TRADERS" envDefault:"2" validate:"gte=1"`
	ClusterMinTotalUSD float64       `env:"CLUSTER_MIN_TOTAL_NOTIONAL_USD" envDefault:"50000" validate:"gte=0"`
	ClusterCooldown    time.Duration `env:"CLUSTER_COOLDOWN" envDefault:"30m" validate:"required"`

	// Volume mode (legacy aggregate-rate detector).
	VolumeMultipliers []float64     `env:"VOLUME_MULTIPLIERS" envDefault:"30,100,1000" envSeparator:","`
	VolumeMinNotional float64       `env:"VOLUME_MIN_NOTIONAL_USD" envDefault:"5000" validate:"gte=0"`
	VolumeMinTrades   int           `env:"VOLUME_MIN_TRADES" envDefault:"5" validate:"gte=0"`
	VolumeCooldown    time.Duration `env:"VOLUME_COOLDOWN" envDefault:"30m" validate:"required"`
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

	GrafanaBaseURL string        `env:"GRAFANA_BASE_URL" envDefault:"http://localhost:3000"`
	GrafanaDashUID string        `env:"GRAFANA_DASH_UID" envDefault:""`
	GrafanaContext time.Duration `env:"GRAFANA_CONTEXT_WINDOW" envDefault:"1h"`
}

type Config struct {
	Application    ApplicationConfig
	Postgres       PostgresConfig
	Backfill       BackfillConfig
	AlertSender    AlertSenderConfig
	Polymarket     PolymarketConfig
	RateLimit      RateLimitConfig
	Pipeline       PipelineConfig
	Aggregate      AggregateConfig
	Anomaly        AnomalyConfig
	CategoryFilter CategoryFilterConfig
	Alerting       AlertingConfig
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
