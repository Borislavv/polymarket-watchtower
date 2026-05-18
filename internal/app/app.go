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
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/persist"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	alerting2 "github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	httpsrv "github.com/Borislavv/polymarket-watchtower/internal/infra/http"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/log"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	pg "github.com/Borislavv/polymarket-watchtower/internal/infra/postgres"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	shutdown2 "github.com/Borislavv/polymarket-watchtower/internal/infra/shutdown"
	"github.com/jackc/pgx/v5/pgxpool"
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

	// pgPool is nil when POSTGRES_DSN is unset. Owned by App so shutdown
	// can drain it cleanly.
	pgPool *pgxpool.Pool
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

	categoryFilter := category.NewFilter(cfg.CategoryFilter.Whitelist)
	logger.Info().Str("category_whitelist", categoryFilter.Summary()).Msg("category filter")

	// Postgres is optional. When DSN is set we open the pool, run
	// migrations, and build a persist.Sink that writes every discovery
	// sweep and trade batch to the DB. When DSN is empty the app stays in
	// Phase-1 mode (in-memory only) — the loops see a nil Persist.
	var (
		pgPool      *pgxpool.Pool
		persistSink *persist.Sink
	)
	if cfg.Postgres.Enabled() {
		if cfg.Postgres.AutoMigrate {
			if err := pg.Migrate(cfg.Postgres.DSN); err != nil {
				return nil, fmt.Errorf("postgres migrate: %w", err)
			}
		}
		var err error
		pgPool, err = pg.Open(context.Background(), pg.Config{
			DSN:             cfg.Postgres.DSN,
			MaxOpenConns:    cfg.Postgres.MaxOpenConns,
			MaxIdleConns:    cfg.Postgres.MaxIdleConns,
			ConnMaxLifetime: cfg.Postgres.ConnMaxLifetime,
		})
		if err != nil {
			return nil, fmt.Errorf("postgres open: %w", err)
		}
		persistSink = persist.NewSink(
			repository.NewCategoryRepository(pgPool),
			repository.NewMarketRepository(pgPool),
			repository.NewTradeRepository(pgPool),
			repository.NewTraderRepository(pgPool),
			cfg.CategoryFilter.Whitelist,
		)
		logger.Info().
			Int("max_open_conns", cfg.Postgres.MaxOpenConns).
			Bool("auto_migrate", cfg.Postgres.AutoMigrate).
			Msg("postgres: persistence enabled")
	} else {
		logger.Info().Msg("postgres: not configured, running in-memory only (Phase-1 mode)")
	}

	discoverCfg := discover.Config{
		Interval:   cfg.Pipeline.DiscoverInterval,
		MaxMarkets: cfg.Pipeline.MaxMarkets,
		ActiveOnly: cfg.Pipeline.ActiveOnly,
		OrderBy:    cfg.Pipeline.OrderBy,
	}
	if persistSink != nil {
		discoverCfg.Persist = persistSink.PersistDiscovery
	}
	discoverLoop := discover.New(discoverCfg, gammaClient, registry, engine, categoryFilter, met, logger)

	sinks := []alerting2.Channel{&alerting2.LogSink{Logger: logger}}
	if cfg.Alerting.WebhookURL != "" {
		sinks = append(sinks, alerting2.NewWebhookSink(cfg.Alerting.WebhookURL))
	}
	telegram, err := alerting2.NewTelegramSink(alerting2.TelegramConfig{
		Enabled:  cfg.Alerting.TelegramEnabled,
		BotToken: cfg.Alerting.TelegramBotToken,
		ChatID:   cfg.Alerting.TelegramChatID,
		BaseURL:  cfg.Alerting.TelegramBaseURL,
		Timeout:  cfg.Alerting.TelegramTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("telegram sink: %w", err)
	}
	telegram.WithMetrics(met)
	sinks = append(sinks, telegram)
	emitter := &alerting2.Fanout{Sinks: sinks, Logger: logger}

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
				Info: anomaly.Tier{
					MinNotionalUSD: cfg.Anomaly.InfoMinNotionalUSD,
					MinOdds:        cfg.Anomaly.InfoMinOdds,
					MinMultiplier:  cfg.Anomaly.InfoMinMultiplier,
				},
				Warning: anomaly.Tier{
					MinNotionalUSD: cfg.Anomaly.WarningMinNotionalUSD,
					MinOdds:        cfg.Anomaly.WarningMinOdds,
					MinMultiplier:  cfg.Anomaly.WarningMinMultiplier,
				},
				Critical: anomaly.Tier{
					MinNotionalUSD: cfg.Anomaly.CriticalMinNotionalUSD,
					MinOdds:        cfg.Anomaly.CriticalMinOdds,
					MinMultiplier:  cfg.Anomaly.CriticalMinMultiplier,
				},
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
			Filter:                      categoryFilter,
			RecentWindows:               cfg.Aggregate.RecentWindows,
			GaugeInterval:               cfg.Pipeline.CollectInterval,
			PolymarketBase:              cfg.Polymarket.PublicBaseURL,
			GrafanaBase:                 cfg.Alerting.GrafanaBaseURL,
			GrafanaDashUID:              cfg.Alerting.GrafanaDashUID,
			GrafanaContext:              cfg.Alerting.GrafanaContext,
			LifecycleAlertFromPct:       cfg.Anomaly.LifecycleAlertFromPct,
			LifecycleHotFromPct:         cfg.Anomaly.LifecycleHotFromPct,
			MarketMinAge:                cfg.Anomaly.MarketMinAge,
			BaselineMinReadySpan:        cfg.Anomaly.BaselineMinReadySpan,
			AllowUnknownMarketLifecycle: cfg.Anomaly.AllowUnknownMarketLifecycle,
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
			Filter:        categoryFilter,
		}, engine, registry, emitter, met, logger)
		observer = volumeLoop
		detectRun = volumeLoop.Run
	default:
		return nil, fmt.Errorf("unknown ANOMALY_MODE %q", cfg.Anomaly.Mode)
	}

	collectCfg := collect.Config{
		Interval:     cfg.Pipeline.CollectInterval,
		Concurrency:  cfg.Pipeline.CollectConcurrency,
		LookbackBoot: longestRecent(cfg.Aggregate.RecentWindows),
	}
	if persistSink != nil {
		collectCfg.Persist = persistSink.PersistTrades
	}
	collectLoop := collect.New(collectCfg, dataClient, engine, registry, observer, met, logger)

	httpSrv := httpsrv.New(cfg.Application.MetricsPort, met.Registry(), logger)

	logger.Info().Str("mode", string(cfg.Anomaly.Mode)).Msg("anomaly detector mode")
	_ = volumeLoop // referenced via detectRun
	_ = detectLoop // referenced via observer / detectRun

	return &App{
		cfg:       cfg,
		logger:    logger,
		metrics:   met,
		registry:  registry,
		engine:    engine,
		discover:  discoverLoop,
		collect:   collectLoop,
		detectRun: detectRun,
		httpSrv:   httpSrv,
		pgPool:    pgPool,
	}, nil
}

func (a *App) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Close the Postgres pool after all workers have drained, so the last
	// queries-in-flight see a live connection.
	defer func() {
		if a.pgPool != nil {
			a.pgPool.Close()
		}
	}()

	execs := []shutdown2.Exec{
		{Name: "metrics-server", Fn: a.httpSrv.Run},
		{Name: "discover", Fn: a.discover.Run},
		{Name: "collect", Fn: a.collect.Run},
		{Name: "detect", Fn: a.detectRun},
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
