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

// ApplicationConfig holds process-wide settings.
type ApplicationConfig struct {
	Env                 Environment   `env:"APP_ENV" envDefault:"dev" validate:"required,oneof=dev local prod"`
	LogLevel            string        `env:"LOG_LEVEL" envDefault:"info" validate:"oneof=trace debug info warn error fatal"`
	MetricsPort         int           `env:"METRICS_PORT" envDefault:"9090" validate:"required,gte=1,lte=65535"`
	ShutdownGracePeriod time.Duration `env:"SHUTDOWN_GRACE_PERIOD" envDefault:"15s" validate:"required"`
}

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

// CategoryFilterConfig holds the category blacklist applied to both modes.
//
// Defaults exclude Polymarket's sports categories — they are high-hype,
// high-volume venues where small baselines and casual whales generate way
// too much noise for the current detector. Operators wanting full coverage
// can set CATEGORY_BLACKLIST= (empty) to disable.
//
// Matching is case-insensitive substring on (slug + label); see the
// internal/app/usecase/category package for the full rule.
type CategoryFilterConfig struct {
	Blacklist []string `env:"CATEGORY_BLACKLIST" envSeparator:"," envDefault:"sport,football,basketball,baseball,hockey,soccer,tennis,golf,mma,boxing,racing,nba,nfl,nhl,mlb,ufc,nascar,cricket,rugby,fifa,uefa,champions league,stanley cup,world cup,epl,wimbledon,grand prix"`
}

// AggregateConfig sizes the rolling-bucket engine used by both modes
// (supporting gauges in single_cluster; alert source in volume).
type AggregateConfig struct {
	BucketSize     time.Duration   `env:"AGG_BUCKET" envDefault:"1m" validate:"required"`
	BaselineWindow time.Duration   `env:"AGG_BASELINE_WINDOW" envDefault:"168h" validate:"required"`
	RecentWindows  []time.Duration `env:"AGG_RECENT_WINDOWS" envDefault:"12h,24h" envSeparator:","`
}

// AnomalyConfig encodes the per-trade single_cluster detector and the
// category-cluster (CategoryWatchRequired / HARD) alert. See score.Score for
// the full semantics of each signal.
type AnomalyConfig struct {
	Mode AnomalyMode `env:"ANOMALY_MODE" envDefault:"single_cluster" validate:"required,oneof=single_cluster volume"`

	SingleMinTradeUSD            float64       `env:"SINGLE_MIN_TRADE_USD" envDefault:"10000" validate:"gte=0"`
	SingleMultiplierThresholds   []float64     `env:"SINGLE_MULTIPLIER_THRESHOLDS" envDefault:"30,100,1000" envSeparator:","`
	SingleOddsThresholds         []float64     `env:"SINGLE_ODDS_THRESHOLDS" envDefault:"3,10,25" envSeparator:","`
	SingleMinBaselineTrades      int           `env:"SINGLE_MIN_BASELINE_TRADES" envDefault:"20" validate:"gte=0"`
	SingleMinBaselineNotionalUSD float64       `env:"SINGLE_MIN_BASELINE_NOTIONAL_USD" envDefault:"1000" validate:"gte=0"`
	BaselineWindow               time.Duration `env:"BASELINE_WINDOW" envDefault:"168h" validate:"required"`
	BaselineMaxSamples           int           `env:"BASELINE_MAX_SAMPLES" envDefault:"1024" validate:"gte=16"`

	ClusterWindow      time.Duration `env:"CLUSTER_WINDOW" envDefault:"30m" validate:"required"`
	ClusterMinTrades   int           `env:"CLUSTER_MIN_ANOMALOUS_TRADES" envDefault:"3" validate:"gte=2"`
	ClusterMinWallets  int           `env:"CLUSTER_MIN_UNIQUE_TRADERS" envDefault:"2" validate:"gte=1"`
	ClusterMinTotalUSD float64       `env:"CLUSTER_MIN_TOTAL_NOTIONAL_USD" envDefault:"30000" validate:"gte=0"`
	ClusterCooldown    time.Duration `env:"CLUSTER_COOLDOWN" envDefault:"30m" validate:"required"`

	// Volume mode (legacy) ---------------------------------------------------
	VolumeMultipliers []float64     `env:"VOLUME_MULTIPLIERS" envDefault:"30,100,1000" envSeparator:","`
	VolumeMinNotional float64       `env:"VOLUME_MIN_NOTIONAL_USD" envDefault:"500" validate:"gte=0"`
	VolumeMinTrades   int           `env:"VOLUME_MIN_TRADES" envDefault:"5" validate:"gte=0"`
	VolumeCooldown    time.Duration `env:"VOLUME_COOLDOWN" envDefault:"30m" validate:"required"`
}

// AlertingConfig selects sinks and provides the Grafana deep-link base.
type AlertingConfig struct {
	WebhookURL              string        `env:"ALERT_WEBHOOK_URL"`
	TelegramEnabled         bool          `env:"TELEGRAM_ENABLED" envDefault:"false"`
	TelegramBotToken        string        `env:"TELEGRAM_BOT_TOKEN"`
	TelegramChatID          string        `env:"TELEGRAM_CHAT_ID"`
	TelegramBaseURL         string        `env:"TELEGRAM_BASE_URL"`
	TelegramTimeout         time.Duration `env:"TELEGRAM_TIMEOUT" envDefault:"5s"`
	TelegramUpdatesEnabled  bool          `env:"TELEGRAM_UPDATES_ENABLED" envDefault:"false"`
	TelegramUpdatesInterval time.Duration `env:"TELEGRAM_UPDATES_INTERVAL" envDefault:"10s"`

	GrafanaBaseURL string        `env:"GRAFANA_BASE_URL" envDefault:"http://localhost:3000"`
	GrafanaDashUID string        `env:"GRAFANA_DASH_UID" envDefault:""`
	GrafanaContext time.Duration `env:"GRAFANA_CONTEXT_WINDOW" envDefault:"1h"`
}

type Config struct {
	Application    ApplicationConfig
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
