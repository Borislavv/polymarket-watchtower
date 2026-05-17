// Package app is the composition root. Nothing else in the codebase should
// build dependencies — they should accept them through their constructors.
package app

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/alerting"
	httpsrv "github.com/Borislavv/polymarket-watchtower/internal/core/infra/http"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/log"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/ratelimit"
	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/shutdown"
	"github.com/Borislavv/polymarket-watchtower/internal/core/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/core/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/core/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/core/usecase/discover"
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

	discover *discover.Loop
	collect  *collect.Loop
	detect   *detect.Loop
	httpSrv  *httpsrv.Server
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

	// HTTP clients — each one gets its own per-host limiter and metrics hook.
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

	discoverLoop := discover.New(discover.Config{
		Interval:   cfg.Pipeline.DiscoverInterval,
		MaxMarkets: cfg.Pipeline.MaxMarkets,
		ActiveOnly: cfg.Pipeline.ActiveOnly,
		OrderBy:    cfg.Pipeline.OrderBy,
	}, gammaClient, registry, engine, met, logger)

	collectLoop := collect.New(collect.Config{
		Interval:     cfg.Pipeline.CollectInterval,
		Concurrency:  cfg.Pipeline.CollectConcurrency,
		LookbackBoot: longestRecent(cfg.Aggregate.RecentWindows),
	}, dataClient, engine, registry, met, logger)

	sinks := []alerting.Sink{&alerting.LogSink{Logger: logger}}
	if cfg.Alerting.WebhookURL != "" {
		sinks = append(sinks, alerting.NewWebhookSink(cfg.Alerting.WebhookURL))
	}
	telegram, err := alerting.NewTelegramSink(alerting.TelegramConfig{
		Enabled:  cfg.Alerting.TelegramEnabled,
		BotToken: cfg.Alerting.TelegramBotToken,
		ChatID:   cfg.Alerting.TelegramChatID,
		BaseURL:  cfg.Alerting.TelegramBaseURL,
		Timeout:  cfg.Alerting.TelegramTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("telegram sink: %w", err)
	}
	sinks = append(sinks, telegram)
	emitter := &alerting.Fanout{Sinks: sinks, Logger: logger}

	detectLoop := detect.New(detect.Config{
		Interval:      cfg.Pipeline.CollectInterval,
		RecentWindows: cfg.Aggregate.RecentWindows,
		Rule: anomaly.Rule{
			Multipliers: cfg.Anomaly.Multipliers,
			MinNotional: cfg.Anomaly.MinVolumeUSD,
			MinTrades:   cfg.Anomaly.MinTrades,
		},
		Cooldown: cfg.Anomaly.CooldownPerRule,
	}, engine, registry, emitter, met, logger)

	httpSrv := httpsrv.New(cfg.Application.MetricsPort, met.Registry(), logger)

	return &App{
		cfg:      cfg,
		logger:   logger,
		metrics:  met,
		registry: registry,
		engine:   engine,
		discover: discoverLoop,
		collect:  collectLoop,
		detect:   detectLoop,
		httpSrv:  httpSrv,
	}, nil
}

func (a *App) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	execs := []shutdown.Exec{
		{Name: "metrics-server", Fn: a.httpSrv.Run},
		{Name: "discover", Fn: a.discover.Run},
		{Name: "collect", Fn: a.collect.Run},
		{Name: "detect", Fn: a.detect.Run},
	}

	return shutdown.Graceful(
		ctx,
		execs,
		shutdown.WithLogger(a.logger),
		shutdown.WithFadeOutDuration(a.cfg.Application.ShutdownGracePeriod),
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
