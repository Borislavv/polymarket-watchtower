// Package app is the composition root. Nothing else in the codebase should
// build dependencies — they should accept them through their constructors.
package app

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/discover"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	alerting2 "github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	httpsrv "github.com/Borislavv/polymarket-watchtower/internal/infra/http"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/log"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
	shutdown2 "github.com/Borislavv/polymarket-watchtower/internal/infra/shutdown"
	"github.com/rs/zerolog"
)

const serviceName = "watchtower"

// App is the wired graph; everything it owns has its lifetime bound to Run.
type App struct {
	cfg    *Config
	logger *zerolog.Logger

	metrics  *metrics.Metrics
	registry *aggregate.MarketRegistry
	engine   *aggregate.Engine

	discover  *discover.Loop
	collect   *collect.Loop
	detectRun func(context.Context) error // active mode's Run
	httpSrv   *httpsrv.Server

	telegramPoller *alerting2.Poller // nil when TELEGRAM_UPDATES_ENABLED=false
}

func New() (*App, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	logger := log.NewWithConfig(log.Config{
		Env:        string(cfg.Application.Env),
		Level:      cfg.Application.LogLevel,
		Service:    serviceName,
		Pretty:     cfg.Application.Env != EnvProd,
		WithCaller: cfg.Application.Env != EnvProd,
	})
	logger.Info().
		Int("cores", runtime.GOMAXPROCS(0)).
		Str("env", string(cfg.Application.Env)).
		Msg("starting watchtower")

	met := metrics.New()
	registry := aggregate.NewRegistry()
	engine := aggregate.New(aggregate.Config{
		Bucket:   cfg.Aggregate.BucketSize,
		Baseline: cfg.Aggregate.BaselineWindow,
	})

	gammaHTTP, err := httpx.New(httpx.Config{
		BaseURL:   cfg.Polymarket.GammaURL,
		Timeout:   cfg.Polymarket.HTTPTimeout,
		UserAgent: cfg.Polymarket.UserAgent,
		Limiter:   ratelimit.New(cfg.RateLimit.GammaPerSec, cfg.RateLimit.GammaBurst),
		Logger:    logger,
		Observe:   met.UpstreamObserver("gamma"),
	})
	if err != nil {
		return nil, fmt.Errorf("gamma http: %w", err)
	}
	dataHTTP, err := httpx.New(httpx.Config{
		BaseURL:   cfg.Polymarket.DataAPIURL,
		Timeout:   cfg.Polymarket.HTTPTimeout,
		UserAgent: cfg.Polymarket.UserAgent,
		Limiter:   ratelimit.New(cfg.RateLimit.DataAPIPerSec, cfg.RateLimit.DataAPIBurst),
		Logger:    logger,
		Observe:   met.UpstreamObserver("dataapi"),
	})
	if err != nil {
		return nil, fmt.Errorf("dataapi http: %w", err)
	}

	gammaClient := gamma.New(gammaHTTP)
	dataClient := dataapi.New(dataHTTP)

	categoryFilter := category.NewFilter(cfg.CategoryFilter.Blacklist)
	logger.Info().Str("category_blacklist", categoryFilter.Summary()).Msg("category filter")

	discoverLoop := discover.New(discover.Config{
		Interval:   cfg.Pipeline.DiscoverInterval,
		MaxMarkets: cfg.Pipeline.MaxMarkets,
		ActiveOnly: cfg.Pipeline.ActiveOnly,
		OrderBy:    cfg.Pipeline.OrderBy,
	}, gammaClient, registry, engine, categoryFilter, met, logger)

	sinks := []alerting2.Channel{&alerting2.LogSink{Logger: logger}}
	if cfg.Alerting.WebhookURL != "" {
		sinks = append(sinks, alerting2.NewWebhookSink(cfg.Alerting.WebhookURL))
	}
	telegramSubs := alerting2.NewSubscribers(cfg.Alerting.TelegramChatID)
	telegram, err := alerting2.NewTelegramSink(alerting2.TelegramConfig{
		Enabled:  cfg.Alerting.TelegramEnabled,
		BotToken: cfg.Alerting.TelegramBotToken,
		ChatID:   cfg.Alerting.TelegramChatID,
		BaseURL:  cfg.Alerting.TelegramBaseURL,
		Timeout:  cfg.Alerting.TelegramTimeout,
	}, telegramSubs)
	if err != nil {
		return nil, fmt.Errorf("telegram sink: %w", err)
	}
	telegram.WithMetrics(met)
	sinks = append(sinks, telegram)
	emitter := &alerting2.Fanout{Sinks: sinks, Logger: logger}

	var telegramPoller *alerting2.Poller
	if cfg.Alerting.TelegramEnabled && cfg.Alerting.TelegramUpdatesEnabled {
		telegramPoller, err = alerting2.NewPoller(alerting2.PollerConfig{
			BotToken: cfg.Alerting.TelegramBotToken,
			BaseURL:  cfg.Alerting.TelegramBaseURL,
			Interval: cfg.Alerting.TelegramUpdatesInterval,
			Timeout:  cfg.Alerting.TelegramTimeout,
		}, telegramSubs, logger)
		if err != nil {
			return nil, fmt.Errorf("telegram poller: %w", err)
		}
	}

	// Detector wiring depends on ANOMALY_MODE. single_cluster is the primary
	// product path (per-trade scoring + cluster HARD alert); volume keeps the
	// legacy aggregate-rate detector for operators who explicitly opt in.
	var (
		detectLoop *detect.Loop
		volumeLoop *detect.VolumeLoop
		observer   collect.TradeObserver
		detectRun  func(context.Context) error
	)
	switch cfg.Anomaly.Mode {
	case ModeSingleCluster:
		detectLoop = detect.New(detect.Config{
			Thresholds: anomaly.Thresholds{
				MultiplierLadder:       cfg.Anomaly.SingleMultiplierThresholds,
				OddsLadder:             cfg.Anomaly.SingleOddsThresholds,
				MinTradeUSD:            cfg.Anomaly.SingleMinTradeUSD,
				MinBaselineTrades:      cfg.Anomaly.SingleMinBaselineTrades,
				MinBaselineNotionalUSD: cfg.Anomaly.SingleMinBaselineNotionalUSD,
			},
			Baseline: baseline.Config{
				Window:     cfg.Anomaly.BaselineWindow,
				MaxSamples: cfg.Anomaly.BaselineMaxSamples,
			},
			Cluster: cluster.Config{
				Window:           cfg.Anomaly.ClusterWindow,
				MinTrades:        cfg.Anomaly.ClusterMinTrades,
				MinUniqueWallets: cfg.Anomaly.ClusterMinWallets,
				MinTotalUSD:      cfg.Anomaly.ClusterMinTotalUSD,
				Cooldown:         cfg.Anomaly.ClusterCooldown,
			},
			Filter:         categoryFilter,
			RecentWindows:  cfg.Aggregate.RecentWindows,
			GaugeInterval:  cfg.Pipeline.CollectInterval,
			PolymarketBase: cfg.Polymarket.PublicBaseURL,
			GrafanaBase:    cfg.Alerting.GrafanaBaseURL,
			GrafanaDashUID: cfg.Alerting.GrafanaDashUID,
			GrafanaContext: cfg.Alerting.GrafanaContext,
		}, engine, registry, emitter, met, logger)
		observer = detectLoop
		detectRun = detectLoop.Run
	case ModeVolume:
		volumeLoop = detect.NewVolume(detect.VolumeConfig{
			Interval:      cfg.Pipeline.CollectInterval,
			RecentWindows: cfg.Aggregate.RecentWindows,
			Multipliers:   cfg.Anomaly.VolumeMultipliers,
			MinNotional:   cfg.Anomaly.VolumeMinNotional,
			MinTrades:     cfg.Anomaly.VolumeMinTrades,
			Cooldown:      cfg.Anomaly.VolumeCooldown,
		}, engine, registry, emitter, met, logger)
		observer = volumeLoop
		detectRun = volumeLoop.Run
	default:
		return nil, fmt.Errorf("unknown ANOMALY_MODE %q", cfg.Anomaly.Mode)
	}

	collectLoop := collect.New(collect.Config{
		Interval:     cfg.Pipeline.CollectInterval,
		Concurrency:  cfg.Pipeline.CollectConcurrency,
		LookbackBoot: longestRecent(cfg.Aggregate.RecentWindows),
	}, dataClient, engine, registry, observer, met, logger)

	httpSrv := httpsrv.New(cfg.Application.MetricsPort, met.Registry(), logger)

	logger.Info().Str("mode", string(cfg.Anomaly.Mode)).Msg("anomaly detector mode")
	_ = volumeLoop // referenced via detectRun
	_ = detectLoop // referenced via observer / detectRun

	return &App{
		cfg:            cfg,
		logger:         logger,
		metrics:        met,
		registry:       registry,
		engine:         engine,
		discover:       discoverLoop,
		collect:        collectLoop,
		detectRun:      detectRun,
		httpSrv:        httpSrv,
		telegramPoller: telegramPoller,
	}, nil
}

func (a *App) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	execs := []shutdown2.Exec{
		{Name: "metrics-server", Fn: a.httpSrv.Run},
		{Name: "discover", Fn: a.discover.Run},
		{Name: "collect", Fn: a.collect.Run},
		{Name: "detect", Fn: a.detectRun},
	}
	if a.telegramPoller != nil {
		execs = append(execs, shutdown2.Exec{Name: "telegram-poller", Fn: a.telegramPoller.Run})
	}

	return shutdown2.Graceful(
		ctx,
		execs,
		shutdown2.WithLogger(a.logger),
		shutdown2.WithFadeOutDuration(a.cfg.Application.ShutdownGracePeriod),
	)
}

// longestRecent returns the longest recent window, which doubles as the
// per-market initial trade lookback so the engine warms up on first tick.
func longestRecent(ws []time.Duration) time.Duration {
	var m time.Duration
	for _, w := range ws {
		if w > m {
			m = w
		}
	}
	if m == 0 {
		m = time.Hour
	}
	return m
}
