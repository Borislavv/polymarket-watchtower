// Package app is the composition root. Nothing else in the codebase should
// build dependencies — they should accept them through their constructors.
package app

import (
	"context"
	"fmt"
	"runtime"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/alertsender"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/accumulation"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/dbbaseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/mmfilter"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/quietmarket"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/traderbaseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/backfill"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/discover"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/drift"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomes"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/persist"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/sanity"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/statsreport"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
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
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

const serviceName = "watchtower"

// App is the wired graph; everything it owns has its lifetime bound to Run.
type App struct {
	cfg    *Config
	logger *zerolog.Logger

	metrics *metrics.Metrics
	cache   *marketcache.Cache

	discover  *discover.Loop
	collect   *collect.Loop
	detectRun func(context.Context) error
	httpSrv   *httpsrv.Server

	// Postgres-backed background workers; nil when DSN is unset.
	backfill *backfill.Worker
	sender   *alertsender.Worker
	sanity   *sanity.Worker
	outcomes *outcomes.Worker
	drift    *drift.Worker
	stats    *statsreport.Worker

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
	cache := marketcache.New()

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

	// Postgres is optional but is the production shape. When DSN is set we
	// open the pool, run migrations, and build:
	//   - persist.Sink: writes discoveries and collected trades through;
	//   - category/market/trade/trader/alert repositories;
	//   - dbbaseline.Provider: the DB-backed baseline read path the
	//     detector queries unconditionally when Postgres is wired
	//     (Strategy v4 removed the BASELINE_SOURCE runtime switch);
	//   - backfill.Worker: fills missing history for new markets;
	//   - alertsender.Worker: delivers persisted alerts to Telegram.
	//
	// When DSN is empty the app stays in-memory only — for local
	// exploration. Production must run with POSTGRES_DSN set; the user
	// guidance is in README.md and presets/README.md.
	var (
		pgPool         *pgxpool.Pool
		persistSink    *persist.Sink
		alertsRepo     *repository.AlertRepository
		marketsRepo    *repository.MarketRepository
		tradesRepo     *repository.TradeRepository
		tradersRepo    *repository.TraderRepository
		dbBaseline     *dbbaseline.Provider
		traderBaseline *traderbaseline.Provider
		mmFilter       *mmfilter.Filter
		backfillWorker *backfill.Worker
		senderWorker   *alertsender.Worker
		sanityWorker   *sanity.Worker
		outcomesWorker *outcomes.Worker
		driftWorker    *drift.Worker
		statsWorker    *statsreport.Worker
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
		marketsRepo = repository.NewMarketRepository(pgPool)
		tradesRepo = repository.NewTradeRepository(pgPool)
		tradersRepo = repository.NewTraderRepository(pgPool)
		alertsRepo = repository.NewAlertRepository(pgPool)
		persistSink = persist.NewSink(
			repository.NewCategoryRepository(pgPool),
			marketsRepo, tradesRepo, tradersRepo,
			cfg.CategoryFilter.Whitelist,
			met,
		)
		dbBaseline = dbbaseline.New(dbbaseline.Config{
			Window: cfg.Anomaly.BaselineWindow,
		}, tradesRepo, marketsRepo)
		traderBaseline = traderbaseline.New(traderbaseline.Config{
			Window: cfg.Anomaly.TraderBaselineWindow,
		}, tradersRepo, tradersRepo)
		mmFilter = mmfilter.New(mmfilter.Config{
			Enabled:          cfg.Anomaly.MMFilterEnabled,
			Lookback:         cfg.Anomaly.MMLookback,
			MinTradesPerSide: cfg.Anomaly.MMMinTradesPerSide,
			NeutralityTol:    cfg.Anomaly.MMNeutralityTol,
		}, tradersRepo, tradersRepo)

		logger.Info().
			Int("max_open_conns", cfg.Postgres.MaxOpenConns).
			Bool("auto_migrate", cfg.Postgres.AutoMigrate).
			Str("baseline_source", "postgres").
			Str("strategy_identity", anomaly.StrategyIdentity).
			Bool("trader_axis_enabled", cfg.Anomaly.MinTraderHistoryTrades > 0).
			Bool("mm_filter_enabled", cfg.Anomaly.MMFilterEnabled).
			Dur("mm_lookback", cfg.Anomaly.MMLookback).
			Bool("accumulation_enabled", cfg.Anomaly.AccumulationEnabled).
			Dur("accumulation_window", cfg.Anomaly.AccumulationWindow).
			Msg("postgres: persistence enabled (production: db-backed)")
	} else {
		logger.Warn().Msg("postgres: POSTGRES_DSN not set — running DEV-ONLY in-memory mode. State is lost on restart, alerts are NOT deduped, accumulation is DISABLED. Do not use in production.")
	}

	discoverCfg := discover.Config{
		Interval:         cfg.Pipeline.DiscoverInterval,
		SafetyMaxMarkets: cfg.Pipeline.DiscoverySafetyMaxMarkets,
		ActiveOnly:       cfg.Pipeline.ActiveOnly,
		OrderBy:          cfg.Pipeline.OrderBy,
	}
	if persistSink != nil {
		discoverCfg.Persist = persistSink.PersistDiscovery
	}
	discoverLoop := discover.New(discoverCfg, gammaClient, cache, categoryFilter, met, logger)

	// Realtime fanout owns log + optional webhook only. Telegram delivery
	// flows through the DB queue and the alertsender worker — keeping it
	// out of this fanout is the "isolated telegram infrastructure" rule.
	// When Postgres is NOT configured (local/debug), the legacy synchronous
	// TelegramSink is added back to the fanout so a developer can still see
	// alerts on Telegram without standing up a database.
	sinks := []alerting2.Channel{&alerting2.LogSink{Logger: logger}}
	if cfg.Alerting.WebhookURL != "" {
		sinks = append(sinks, alerting2.NewWebhookSink(cfg.Alerting.WebhookURL))
	}
	if !cfg.Postgres.Enabled() && cfg.Alerting.TelegramEnabled {
		legacyTg, err := alerting2.NewTelegramSink(alerting2.TelegramConfig{
			Enabled:  cfg.Alerting.TelegramEnabled,
			BotToken: cfg.Alerting.TelegramBotToken,
			ChatID:   cfg.Alerting.TelegramChatID,
			BaseURL:  cfg.Alerting.TelegramBaseURL,
			Timeout:  cfg.Alerting.TelegramTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("telegram sink: %w", err)
		}
		legacyTg.WithMetrics(met)
		sinks = append(sinks, legacyTg)
	}
	emitter := &alerting2.Fanout{Sinks: sinks, Logger: logger}

	// Backfill + sender workers exist only when Postgres is wired. The
	// sender worker also needs Telegram enabled with a valid recipient —
	// failing fast at boot is the spec.
	if cfg.Postgres.Enabled() && cfg.Alerting.TelegramEnabled {
		bot, err := telegram.New(telegram.Config{
			BotToken: cfg.Alerting.TelegramBotToken,
			BaseURL:  cfg.Alerting.TelegramBaseURL,
			Timeout:  cfg.Alerting.TelegramTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("telegram bot: %w", err)
		}
		if cfg.Alerting.TelegramChatID == "" {
			return nil, fmt.Errorf("telegram: chat id required when enabled (TELEGRAM_CHAT_ID)")
		}
		senderWorker = alertsender.New(alertsender.Config{
			Interval:          cfg.AlertSender.Interval,
			ClaimLimit:        cfg.AlertSender.ClaimLimit,
			Workers:           cfg.AlertSender.Workers,
			ChatID:            cfg.Alerting.TelegramChatID,
			StaleSendingAfter: cfg.AlertSender.StaleSendingAfter,
			Retry: alertsender.RetryPolicy{
				Enabled:        cfg.AlertSender.RetryEnabled,
				MaxAttempts:    cfg.AlertSender.RetryMaxAttempts,
				InitialBackoff: cfg.AlertSender.RetryInitialBackoff,
				MaxBackoff:     cfg.AlertSender.RetryMaxBackoff,
				JitterFraction: cfg.AlertSender.RetryJitterFraction,
			},
		}, alertsRepo, bot, met, logger)
		logger.Info().
			Str("chat_id", cfg.Alerting.TelegramChatID).
			Int("workers", cfg.AlertSender.Workers).
			Msg("alertsender: enabled")

		if cfg.StatsReport.Enabled {
			statsWorker = statsreport.New(statsreport.Config{
				Interval:     cfg.StatsReport.Interval,
				ChatID:       cfg.Alerting.TelegramChatID,
				StartupGrace: cfg.StatsReport.StartupGrace,
			}, statsreport.NewStore(pgPool), telegramSenderAdapter{bot: bot}, met, logger)
			logger.Info().
				Dur("interval", cfg.StatsReport.Interval).
				Msg("statsreport: enabled (periodic Telegram summary)")
		}
	}
	if cfg.Postgres.Enabled() {
		backfillWorker = backfill.New(backfill.Config{
			Interval:    cfg.Backfill.Interval,
			BatchSize:   cfg.Backfill.Workers,
			Concurrency: cfg.Backfill.Workers,
			PageSize:    cfg.Backfill.PageLimit,
			StaleAfter:  cfg.Backfill.StaleAfter,
		}, marketsRepo, tradesRepo, tradersRepo, dataClient, met, logger)
		logger.Info().
			Int("workers", cfg.Backfill.Workers).
			Dur("interval", cfg.Backfill.Interval).
			Msg("backfill: enabled")

		sanityWorker = sanity.New(sanity.Config{
			Interval:   cfg.MarketSanity.Interval,
			Retention:  cfg.MarketSanity.Retention,
			ClaimLimit: int32(cfg.MarketSanity.ClaimLimit),
		}, marketsRepo, marketcacheUpstream{cache: cache}, met, logger)
		logger.Info().
			Dur("interval", cfg.MarketSanity.Interval).
			Dur("retention", cfg.MarketSanity.Retention).
			Msg("sanity: enabled (soft-delete reaper)")

		if cfg.Outcomes.Enabled {
			outcomesWorker = outcomes.New(outcomes.Config{
				Interval:              cfg.Outcomes.Interval,
				ClaimLimit:            int32(cfg.Outcomes.ClaimLimit),
				WinningPriceThreshold: cfg.Outcomes.WinningPriceThreshold,
			}, alertsRepo, marketsRepo, gammaClient, logger)
			logger.Info().
				Dur("interval", cfg.Outcomes.Interval).
				Msg("outcomes: enabled (post-alert resolution tracking)")
		}
		if cfg.Drift.Enabled {
			driftWorker = drift.New(drift.Config{
				Interval:      cfg.Drift.Interval,
				ClaimLimit:    int32(cfg.Drift.ClaimLimit),
				LongestWindow: 24 * 60 * 60 * 1_000_000_000, // 24h hardcoded to match column set
			}, alertsRepo, tradesRepo, logger)
			logger.Info().
				Dur("interval", cfg.Drift.Interval).
				Msg("drift: enabled (CLV-lite post-trade enrichment)")
		}
	}

	// Detection is the single_cluster pipeline: per-trade scoring +
	// cluster + accumulation + quiet-market wake-up. Volume mode and
	// the supporting in-memory aggregate engine were removed in the v4
	// cleanup — the production decision source is dbbaseline.Provider.
	detectCfg := detect.Config{
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
			Window: cfg.Anomaly.BaselineWindow,
		},
		Cluster: cluster.Config{
			Window:           cfg.Anomaly.ClusterWindow,
			MinTrades:        cfg.Anomaly.ClusterMinTrades,
			MinUniqueWallets: cfg.Anomaly.ClusterMinWallets,
			MinTotalUSD:      cfg.Anomaly.ClusterMinTotalUSD,
			Cooldown:         cfg.Anomaly.ClusterCooldown,
		},
		Filter:                categoryFilter,
		PolymarketBase:        cfg.Polymarket.PublicBaseURL,
		GrafanaBase:           cfg.Alerting.GrafanaBaseURL,
		GrafanaDashUID:        cfg.Alerting.GrafanaDashUID,
		GrafanaContext:        cfg.Alerting.GrafanaContext,
		LifecycleAlertFromPct: cfg.Anomaly.LifecycleAlertFromPct,
		LifecycleHotFromPct:   cfg.Anomaly.LifecycleHotFromPct,
		MarketMinAge:          cfg.Anomaly.MarketMinAge,
		BaselineMinReadySpan:  cfg.Anomaly.BaselineMinReadySpan,
		StrategyVersion:       anomaly.StrategyIdentity,
	}
	// Production: DB baseline + DB-backed alert dedup. The detector only
	// uses the in-memory reservoir embedded in baseline.Baseline when
	// Postgres is unwired (dev/debug only).
	if cfg.Postgres.Enabled() {
		detectCfg.Baseliner = dbBaseline
		detectCfg.Alerts = alertsRepo
		detectCfg.Markets = marketsRepo
		detectCfg.Traders = tradersRepo
		detectCfg.TraderBaseliner = traderBaseline
		detectCfg.MinTraderHistoryTrades = cfg.Anomaly.MinTraderHistoryTrades
		if mmFilter != nil {
			detectCfg.MMFilter = mmFilter
		}
		if cfg.Anomaly.QuietMarketEnabled {
			detectCfg.QuietMarket = quietmarket.New(quietmarket.Config{
				Enabled:               true,
				MaxTradesPerDay:       cfg.Anomaly.QuietMarketMaxTradesPerDay,
				MaxNotionalPerDayUSD:  cfg.Anomaly.QuietMarketMaxNotionalPerDay,
				MinIdleDuration:       cfg.Anomaly.QuietMarketMinIdleDuration,
				MinCurrentNotionalUSD: cfg.Anomaly.QuietMarketMinCurrentNotional,
				MinMultiplier:         cfg.Anomaly.QuietMarketMinMultiplier,
			})
			detectCfg.LastTradeFetcher = tradesRepo
		}
		if cfg.Anomaly.AccumulationEnabled {
			detectCfg.Accumulator = accumulation.New(accumulation.Config{
				Enabled:              true,
				Window:               cfg.Anomaly.AccumulationWindow,
				MinTrades:            cfg.Anomaly.AccumulationMinTrades,
				TradeFractionOfInfo:  cfg.Anomaly.AccumulationMinTradeFraction,
				TotalMultiplier:      cfg.Anomaly.AccumulationTotalMultiplier,
				ManySmallsMultiplier: cfg.Anomaly.AccumulationManySmallsMultiplier,
				HardMultiplier:       cfg.Anomaly.AccumulationHardMultiplier,
				Cooldown:             cfg.Anomaly.AccumulationCooldown,
			}, detectCfg.Thresholds)
			detectCfg.AccumulationLines = tradesRepo
		}
	}
	detectLoop := detect.New(detectCfg, cache, emitter, met, logger)

	collectCfg := collect.Config{
		Interval:          cfg.Pipeline.CollectInterval,
		Concurrency:       cfg.Pipeline.CollectConcurrency,
		BootstrapLookback: cfg.Pipeline.CollectBootstrapLookback,
	}
	if persistSink != nil {
		collectCfg.Persist = persistSink.PersistTrades
		collectCfg.Cursor = persistSink.LatestTradedAt
	}
	collectLoop := collect.New(collectCfg, dataClient, cache, detectLoop, met, logger)

	httpSrv := httpsrv.New(cfg.Application.MetricsPort, met.Registry(), logger)

	return &App{
		cfg:       cfg,
		logger:    logger,
		metrics:   met,
		cache:     cache,
		discover:  discoverLoop,
		collect:   collectLoop,
		detectRun: detectLoop.Run,
		httpSrv:   httpSrv,
		backfill:  backfillWorker,
		sender:    senderWorker,
		sanity:    sanityWorker,
		outcomes:  outcomesWorker,
		drift:     driftWorker,
		stats:     statsWorker,
		pgPool:    pgPool,
	}, nil
}

// telegramSenderAdapter narrows *telegram.Bot to statsreport.Sender by
// dropping the SendResult — the stats worker has no use for the
// upstream Telegram message id.
type telegramSenderAdapter struct{ bot *telegram.Bot }

func (a telegramSenderAdapter) SendHTML(ctx context.Context, chatID, text string) error {
	_, err := a.bot.SendHTML(ctx, chatID, text)
	return err
}

// marketcacheUpstream adapts *marketcache.Cache to sanity.UpstreamChecker.
// "Active upstream" is exactly "present in the most recent discover
// sweep" — which is what the cache contains. Cache miss → market is no
// longer upstream → returns false → sanity proceeds with purge.
type marketcacheUpstream struct{ cache *marketcache.Cache }

func (u marketcacheUpstream) IsActiveUpstream(conditionID string) bool {
	if u.cache == nil {
		return false
	}
	_, ok := u.cache.Get(vo.MarketID(conditionID))
	return ok
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
	if a.backfill != nil {
		execs = append(execs, shutdown2.Exec{Name: "backfill", Fn: a.backfill.Run})
	}
	if a.sender != nil {
		execs = append(execs, shutdown2.Exec{Name: "alertsender", Fn: a.sender.Run})
	}
	if a.sanity != nil {
		execs = append(execs, shutdown2.Exec{Name: "sanity", Fn: a.sanity.Run})
	}
	if a.outcomes != nil {
		execs = append(execs, shutdown2.Exec{Name: "outcomes", Fn: a.outcomes.Run})
	}
	if a.drift != nil {
		execs = append(execs, shutdown2.Exec{Name: "drift", Fn: a.drift.Run})
	}
	if a.stats != nil {
		execs = append(execs, shutdown2.Exec{Name: "statsreport", Fn: a.stats.Run})
	}

	return shutdown2.Graceful(
		ctx,
		execs,
		shutdown2.WithLogger(a.logger),
		shutdown2.WithFadeOutDuration(a.cfg.Application.ShutdownGracePeriod),
	)
}
