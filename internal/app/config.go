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

// PolymarketConfig points at upstream APIs.
type PolymarketConfig struct {
	GammaURL    string        `env:"GAMMA_API_URL" envDefault:"https://gamma-api.polymarket.com" validate:"required,url"`
	DataAPIURL  string        `env:"DATA_API_URL" envDefault:"https://data-api.polymarket.com" validate:"required,url"`
	CLOBURL     string        `env:"CLOB_API_URL" envDefault:"https://clob.polymarket.com" validate:"required,url"`
	HTTPTimeout time.Duration `env:"POLYMARKET_HTTP_TIMEOUT" envDefault:"15s" validate:"required"`
	UserAgent   string        `env:"POLYMARKET_USER_AGENT" envDefault:"polymarket-watchtower/0.1"`
	// PublicBaseURL is the user-facing site used in alert deep-links.
	PublicBaseURL string `env:"POLYMARKET_PUBLIC_BASE_URL" envDefault:"https://polymarket.com"`
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

// AggregateConfig sizes the supporting rolling-bucket engine (Grafana gauges
// only — no longer drives alerts).
type AggregateConfig struct {
	BucketSize     time.Duration   `env:"AGG_BUCKET" envDefault:"1m" validate:"required"`
	BaselineWindow time.Duration   `env:"AGG_BASELINE_WINDOW" envDefault:"168h" validate:"required"`
	RecentWindows  []time.Duration `env:"AGG_RECENT_WINDOWS" envDefault:"12h,24h" envSeparator:","`
}

// AnomalyConfig encodes the per-trade detector and the category-cluster
// (CategoryWatchRequired / HARD) alert.
//
// Two independent single-trade ladders, higher severity wins:
//
//   - Multipliers: (trade USD / baseline median USD), evaluated only when the
//     bucket has at least MinBaselineTrades samples. Mapping
//     [info, warning, critical] = [30, 100, 1000]× by default.
//
//   - AbsoluteUSDTiers: absolute USD ladder on the trade's notional, evaluated
//     regardless of baseline. Defaults [$3k, $10k, $100k] = [info, warning,
//     critical]. These match the product example "$10 → $3k/$10k/$100k"
//     even when no baseline exists yet.
//
// Cluster: in HardAlertWindow (default 1h), if a single category sees at least
// HardAlertMinTrades anomalous trades from at least HardAlertMinWallets unique
// wallets totalling at least HardAlertMinTotalUSD, emit HARD alert with cooldown.
type AnomalyConfig struct {
	Multipliers        []float64     `env:"SINGLE_TRADE_MULTIPLIERS" envDefault:"30,100,1000" envSeparator:","`
	AbsoluteUSDTiers   []float64     `env:"SINGLE_TRADE_ABSOLUTE_USD" envDefault:"3000,10000,100000" envSeparator:","`
	MinBaselineTrades  int           `env:"MIN_BASELINE_TRADES" envDefault:"20" validate:"gte=0"`
	BaselineWindow     time.Duration `env:"BASELINE_WINDOW" envDefault:"168h" validate:"required"`
	BaselineMaxSamples int           `env:"BASELINE_MAX_SAMPLES" envDefault:"1024" validate:"gte=16"`

	HardAlertWindow      time.Duration `env:"HARD_ALERT_WINDOW" envDefault:"1h" validate:"required"`
	HardAlertMinTrades   int           `env:"HARD_ALERT_MIN_ANOMALOUS_TRADES" envDefault:"5" validate:"gte=2"`
	HardAlertMinWallets  int           `env:"HARD_ALERT_MIN_UNIQUE_TRADERS" envDefault:"3" validate:"gte=1"`
	HardAlertMinTotalUSD float64       `env:"HARD_ALERT_MIN_TOTAL_NOTIONAL_USD" envDefault:"25000" validate:"gte=0"`
	HardAlertCooldown    time.Duration `env:"HARD_ALERT_COOLDOWN" envDefault:"1h" validate:"required"`
}

// AlertingConfig selects sinks and provides the Grafana deep-link base.
//
// Telegram chats are addressed via a union of two sources:
//   - TELEGRAM_CHAT_ID: optional static chat (always included if non-empty).
//   - Dynamic subscribers discovered by polling /getUpdates when
//     TELEGRAM_UPDATES_ENABLED=true. The bot must be interacted with (or
//     added to a group/channel) at least once for it to learn the chat id.
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
