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

// ApplicationConfig holds process-wide settings.
type ApplicationConfig struct {
	Env                 Environment   `env:"APP_ENV" envDefault:"dev" validate:"required,oneof=dev local prod"`
	LogLevel            string        `env:"LOG_LEVEL" envDefault:"info" validate:"oneof=trace debug info warn error fatal"`
	MetricsPort         int           `env:"METRICS_PORT" envDefault:"9090" validate:"required,gte=1,lte=65535"`
	ShutdownGracePeriod time.Duration `env:"SHUTDOWN_GRACE_PERIOD" envDefault:"15s" validate:"required"`
}

// PolymarketConfig points at upstream APIs. Defaults are the public hosts.
type PolymarketConfig struct {
	GammaURL    string        `env:"GAMMA_API_URL" envDefault:"https://gamma-api.polymarket.com" validate:"required,url"`
	DataAPIURL  string        `env:"DATA_API_URL" envDefault:"https://data-api.polymarket.com" validate:"required,url"`
	CLOBURL     string        `env:"CLOB_API_URL" envDefault:"https://clob.polymarket.com" validate:"required,url"`
	HTTPTimeout time.Duration `env:"POLYMARKET_HTTP_TIMEOUT" envDefault:"15s" validate:"required"`
	UserAgent   string        `env:"POLYMARKET_USER_AGENT" envDefault:"polymarket-watchtower/0.1"`
}

// RateLimitConfig holds per-host caps. Defaults sit at ~70% of the documented
// 10s budgets to leave headroom for burst and other consumers.
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

// AggregateConfig sizes the rolling-bucket engine.
type AggregateConfig struct {
	BucketSize     time.Duration   `env:"AGG_BUCKET" envDefault:"1m" validate:"required"`
	BaselineWindow time.Duration   `env:"AGG_BASELINE_WINDOW" envDefault:"168h" validate:"required"` // 7d
	RecentWindows  []time.Duration `env:"AGG_RECENT_WINDOWS" envDefault:"12h,24h" envSeparator:","`
}

// AnomalyConfig encodes the spike rules. A finding fires when recent rate
// (per-minute) divided by baseline rate (per-minute) exceeds Multiplier and
// the recent volume exceeds MinVolume (USD).
type AnomalyConfig struct {
	Multipliers     []float64     `env:"ANOMALY_MULTIPLIERS" envDefault:"30,100,1000" envSeparator:","`
	MinVolumeUSD    float64       `env:"ANOMALY_MIN_VOLUME_USD" envDefault:"500" validate:"gte=0"`
	MinTrades       int           `env:"ANOMALY_MIN_TRADES" envDefault:"5" validate:"gte=0"`
	CooldownPerRule time.Duration `env:"ANOMALY_COOLDOWN" envDefault:"30m" validate:"required"`
}

// AlertingConfig selects sinks for findings.
type AlertingConfig struct {
	WebhookURL       string        `env:"ALERT_WEBHOOK_URL"`
	TelegramEnabled  bool          `env:"TELEGRAM_ENABLED" envDefault:"false"`
	TelegramBotToken string        `env:"TELEGRAM_BOT_TOKEN"`
	TelegramChatID   string        `env:"TELEGRAM_CHAT_ID"`
	TelegramBaseURL  string        `env:"TELEGRAM_BASE_URL"`
	TelegramTimeout  time.Duration `env:"TELEGRAM_TIMEOUT" envDefault:"5s"`
}

type Config struct {
	Application ApplicationConfig
	Polymarket  PolymarketConfig
	RateLimit   RateLimitConfig
	Pipeline    PipelineConfig
	Aggregate   AggregateConfig
	Anomaly     AnomalyConfig
	Alerting    AlertingConfig
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
