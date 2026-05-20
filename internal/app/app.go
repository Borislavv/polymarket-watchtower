// Package app is the composition root. Nothing else in the codebase should
// build dependencies — they should accept them through their constructors.
package app

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aianalysis"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/alertsender"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/accumulation"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/dbbaseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/mmfilter"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/ownership"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/quietmarket"
	sfdet "github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/stablefavorite"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/traderbaseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/annotationranking"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/backfill"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/dailypoliticalintel"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detection"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/discover"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/drift"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventcatalyst"
	catalystimporter "github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventcatalyst/importer"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketintel"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction/create"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction/evolution"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomeai"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomes"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/persist"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/predictionarchival"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/predictionfeedback"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/sanity"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/signalreport"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/stablefavorite"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/statsreport"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/aibudget"
	alerting2 "github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	httpsrv "github.com/Borislavv/polymarket-watchtower/internal/infra/http"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/log"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	pg "github.com/Borislavv/polymarket-watchtower/internal/infra/postgres"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
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
	backfill          *backfill.Worker
	sender            *alertsender.Worker
	sanity            *sanity.Worker
	outcomes          *outcomes.Worker
	drift             *drift.Worker
	stats             *statsreport.Worker
	signalReport      *signalreport.Worker
	detection         *detection.Worker
	stableFavorite    *stablefavorite.Worker
	aiAnalysis        *aianalysis.Service
	outcomeAI         *outcomeai.Worker
	marketIntel       *marketintel.Worker
	catalystImporter  *catalystimporter.Worker
	dailyIntel        *dailypoliticalintel.Worker
	predictionEvolver *evolution.Worker
	predictionCreator *create.Worker
	predictionFeedbk  *predictionfeedback.Worker
	predictionArchive *predictionarchival.Worker

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
		pgPool             *pgxpool.Pool
		persistSink        *persist.Sink
		alertsRepo         *repository.AlertRepository
		alertAnalysisRepo  *repository.AlertAnalysisRepository
		marketsRepo        *repository.MarketRepository
		tradesRepo         *repository.TradeRepository
		tradersRepo        *repository.TraderRepository
		dbBaseline         *dbbaseline.Provider
		traderBaseline     *traderbaseline.Provider
		mmFilter           *mmfilter.Filter
		backfillWorker     *backfill.Worker
		senderWorker       *alertsender.Worker
		sanityWorker       *sanity.Worker
		outcomesWorker     *outcomes.Worker
		driftWorker        *drift.Worker
		statsWorker        *statsreport.Worker
		signalReportWorker *signalreport.Worker
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
		alertAnalysisRepo = repository.NewAlertAnalysisRepository(pgPool)
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
	//
	// `bot` is hoisted into the outer Postgres-enabled block so the
	// outcomes-reactor pass and the signal-report worker can both
	// reach the same handle without re-constructing it.
	var bot *telegram.Bot
	if cfg.Postgres.Enabled() && cfg.Alerting.TelegramEnabled {
		var err error
		bot, err = telegram.New(telegram.Config{
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

		if cfg.SignalReport.Enabled {
			loc, err := time.LoadLocation(cfg.SignalReport.Timezone)
			if err != nil {
				return nil, fmt.Errorf("signal_reports: invalid timezone %q: %w", cfg.SignalReport.Timezone, err)
			}
			sendAt := map[signalreport.PeriodType]signalreport.TimeOfDay{}
			for k, raw := range map[signalreport.PeriodType]string{
				signalreport.PeriodDaily:     cfg.SignalReport.DailyAt,
				signalreport.PeriodWeekly:    cfg.SignalReport.WeeklyAt,
				signalreport.PeriodMonthly:   cfg.SignalReport.MonthlyAt,
				signalreport.PeriodQuarterly: cfg.SignalReport.QuarterlyAt,
				signalreport.PeriodYearly:    cfg.SignalReport.YearlyAt,
			} {
				tod, err := signalreport.ParseTimeOfDay(raw)
				if err != nil {
					return nil, fmt.Errorf("signal_reports: %s: %w", k, err)
				}
				sendAt[k] = tod
			}
			signalReportWorker = signalreport.New(signalreport.Config{
				Enabled:      true,
				Location:     loc,
				ChatID:       cfg.Alerting.TelegramChatID,
				TickInterval: cfg.SignalReport.TickInterval,
				SendAt:       sendAt,
				YearlyDelay:  cfg.SignalReport.YearlyDelay,
			}, repository.NewSignalReportRepository(pgPool),
				telegramSignalSenderAdapter{bot: bot},
				signalReportMetricsAdapter{m: met},
				logger)
			logger.Info().
				Str("timezone", cfg.SignalReport.Timezone).
				Dur("yearly_delay", cfg.SignalReport.YearlyDelay).
				Msg("signalreport: enabled (signal-quality reports)")
		}
	}
	if cfg.Postgres.Enabled() {
		backfillWorker = backfill.New(backfill.Config{
			Interval:          cfg.Backfill.Interval,
			BatchSize:         cfg.Backfill.Workers,
			Concurrency:       cfg.Backfill.Workers,
			PageSize:          cfg.Backfill.PageLimit,
			StaleAfter:        cfg.Backfill.StaleAfter,
			PartialRetryAfter: cfg.Backfill.PartialRetryAfter,
		}, marketsRepo, tradesRepo, tradersRepo, dataClient, met, logger)
		logger.Info().
			Int("workers", cfg.Backfill.Workers).
			Int("page_limit", cfg.Backfill.PageLimit).
			Dur("interval", cfg.Backfill.Interval).
			Dur("stale_after", cfg.Backfill.StaleAfter).
			Dur("partial_retry_after", cfg.Backfill.PartialRetryAfter).
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
			// The reactor pass only wires when Telegram is configured
			// (it needs a Bot to call setMessageReaction). When it's
			// not wired, the outcomes worker still classifies — rows
			// just stay in telegram_reaction_status='pending' until an
			// operator flips reactions back on.
			reactionsCfg := outcomes.ReactionsConfig{}
			if cfg.TelegramReactions.Enabled && bot != nil && cfg.Alerting.TelegramChatID != "" {
				reactionsCfg = outcomes.ReactionsConfig{
					Enabled:          true,
					ChatID:           cfg.Alerting.TelegramChatID,
					SuccessEmoji:     cfg.TelegramReactions.SuccessEmoji,
					FailureEmoji:     cfg.TelegramReactions.FailureEmoji,
					AmbiguousEmoji:   cfg.TelegramReactions.AmbiguousEmoji,
					DisableAmbiguous: cfg.TelegramReactions.DisableAmbiguous,
					Bot:              bot,
					Metrics:          reactionMetricsAdapter{m: met},
				}
			}
			outcomesWorker = outcomes.New(outcomes.Config{
				Interval:              cfg.Outcomes.Interval,
				ClaimLimit:            int32(cfg.Outcomes.ClaimLimit),
				WinningPriceThreshold: cfg.Outcomes.WinningPriceThreshold,
				Reactions:             reactionsCfg,
				OutcomeMetrics:        outcomeMetricsAdapter{m: met},
			}, alertsRepo, marketsRepo, gammaClient, logger)
			logger.Info().
				Dur("interval", cfg.Outcomes.Interval).
				Bool("reactions_enabled", reactionsCfg.Enabled).
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
				MinNotionalUSD:    cfg.Anomaly.InfoMinNotionalUSD,
				MinOdds:           cfg.Anomaly.InfoMinOdds,
				MinProfitUSD:      cfg.Anomaly.InfoMinProfitUSD,
				MinMarketP95Ratio: cfg.Anomaly.InfoMinMarketP95Ratio,
				MinMarketP99Ratio: cfg.Anomaly.InfoMinMarketP99Ratio,
				MinTraderP95Ratio: cfg.Anomaly.InfoMinTraderP95Ratio,
				MinTraderP99Ratio: cfg.Anomaly.InfoMinTraderP99Ratio,
				MinMultiplier:     cfg.Anomaly.InfoMinMultiplier,
			},
			Warning: anomaly.Tier{
				MinNotionalUSD:    cfg.Anomaly.WarningMinNotionalUSD,
				MinOdds:           cfg.Anomaly.WarningMinOdds,
				MinProfitUSD:      cfg.Anomaly.WarningMinProfitUSD,
				MinMarketP95Ratio: cfg.Anomaly.WarningMinMarketP95Ratio,
				MinMarketP99Ratio: cfg.Anomaly.WarningMinMarketP99Ratio,
				MinTraderP95Ratio: cfg.Anomaly.WarningMinTraderP95Ratio,
				MinTraderP99Ratio: cfg.Anomaly.WarningMinTraderP99Ratio,
				MinMultiplier:     cfg.Anomaly.WarningMinMultiplier,
			},
			Critical: anomaly.Tier{
				MinNotionalUSD:    cfg.Anomaly.CriticalMinNotionalUSD,
				MinOdds:           cfg.Anomaly.CriticalMinOdds,
				MinProfitUSD:      cfg.Anomaly.CriticalMinProfitUSD,
				MinMarketP95Ratio: cfg.Anomaly.CriticalMinMarketP95Ratio,
				MinMarketP99Ratio: cfg.Anomaly.CriticalMinMarketP99Ratio,
				MinTraderP95Ratio: cfg.Anomaly.CriticalMinTraderP95Ratio,
				MinTraderP99Ratio: cfg.Anomaly.CriticalMinTraderP99Ratio,
				MinMultiplier:     cfg.Anomaly.CriticalMinMultiplier,
			},
			MinBaselineTrades:                cfg.Anomaly.SingleMinBaselineTrades,
			MinBaselineNotionalUSD:           cfg.Anomaly.SingleMinBaselineNotionalUSD,
			LowBaselineCapEnabled:            cfg.Anomaly.LowBaselineCapEnabled,
			LowBaselineSingleMaxSeverity:     anomaly.Severity(cfg.Anomaly.LowBaselineSingleMaxSeverity),
			LowBaselineAllowCriticalAbsolute: cfg.Anomaly.LowBaselineAllowCriticalAbsolute,
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
		LiveAlertMaxLag:       cfg.Anomaly.LiveAlertMaxLag,
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
		// Strategy E (market-ownership concentration) wires whenever
		// Postgres is configured AND the operator hasn't disabled it.
		// The detector is invoked from the accumulation path so it
		// can never spam on its own.
		if cfg.Anomaly.OwnershipEnabled {
			detectCfg.Ownership = ownership.New(ownership.Config{
				Enabled:        true,
				InfoPct:        cfg.Anomaly.OwnershipInfoPct,
				WarningPct:     cfg.Anomaly.OwnershipWarningPct,
				CriticalPct:    cfg.Anomaly.OwnershipCriticalPct,
				MinNotionalUSD: cfg.Anomaly.OwnershipMinNotionalUSD,
			})
			detectCfg.OwnershipShares = tradesRepo
		}
		// Strategy B (new-wallet context booster). Reads Trader.FirstSeenAt
		// through cfg.Traders (already wired above for the alert FK), so
		// no new DB dependency.
		detectCfg.NewWallet = detect.NewWalletConfig{
			Enabled:          cfg.Anomaly.NewWalletEnabled,
			MaxAge:           cfg.Anomaly.NewWalletMaxAge,
			MaxHistoryTrades: cfg.Anomaly.NewWalletMaxHistoryTrades,
		}
		// Dormant-wallet booster (v6) — context only, never standalone.
		detectCfg.DormantWallet = detect.DormantWalletConfig{
			Enabled:        cfg.Anomaly.DormantWalletEnabled,
			MinIdle:        cfg.Anomaly.DormantWalletMinIdle,
			MinNotionalUSD: cfg.Anomaly.DormantWalletMinNotionalUSD,
		}
		detectCfg.TraderActivity = tradesRepo
	}
	detectLoop := detect.New(detectCfg, cache, emitter, met, logger)

	// Detection worker — v6 architecture. When Postgres is wired, every
	// persisted trade lands as 'pending' and the worker drains it
	// through detectLoop.Observe. The collect loop no longer calls
	// Observe inline; that path lives only in the memory-only dev mode
	// where there's no queue to drain.
	//
	// IMPORTANT: collectObserver MUST be declared as the interface
	// type, not *detect.Loop, so the Postgres branch can assign a true
	// nil-interface. A typed-nil pointer boxed into an interface value
	// is NOT nil under `!= nil` — the previous shape stored
	// (type=*detect.Loop, value=nil) into collect.Loop.observer and
	// caused (*detect.Loop).Observe to be invoked on a nil receiver
	// for every trade in Postgres mode (incident: detect.go SIGSEGV at
	// the first l.metrics field load).
	var (
		detectionWorker *detection.Worker
		collectObserver collect.TradeObserver = detectLoop // memory-mode default
		detectMode                            = "inline_memory"
	)
	if cfg.Postgres.Enabled() {
		dw := detection.New(detection.Config{
			Workers:        cfg.Detection.Workers,
			ClaimLimit:     int32(cfg.Detection.ClaimLimit),
			Interval:       cfg.Detection.Interval,
			ClaimTTL:       cfg.Detection.ClaimTTL,
			StaleThreshold: cfg.Anomaly.LiveAlertMaxLag,
		}, tradesRepo, cache, detectLoop, walletResolver(tradersRepo), met, logger)
		detectionWorker = dw
		// Collect no longer drives detection in production. Assigning
		// nil to a variable already typed as collect.TradeObserver
		// produces a true nil-interface value — the guard in
		// collect.pull (`if l.observer != nil`) now skips correctly.
		collectObserver = nil
		detectMode = "db_queue"
		logger.Info().
			Str("detect_mode", detectMode).
			Int("workers", cfg.Detection.Workers).
			Int("claim_limit", cfg.Detection.ClaimLimit).
			Dur("interval", cfg.Detection.Interval).
			Dur("claim_ttl", cfg.Detection.ClaimTTL).
			Dur("stale_threshold", cfg.Anomaly.LiveAlertMaxLag).
			Msg("detection worker: enabled")
	} else {
		logger.Info().Str("detect_mode", detectMode).Msg("detection pipeline wired (memory mode)")
	}

	collectCfg := collect.Config{
		Interval:          cfg.Pipeline.CollectInterval,
		Concurrency:       cfg.Pipeline.CollectConcurrency,
		BootstrapLookback: cfg.Pipeline.CollectBootstrapLookback,
	}
	if persistSink != nil {
		collectCfg.Persist = persistSink.PersistTrades
		collectCfg.Cursor = persistSink.LatestTradedAt
	}
	collectLoop := collect.New(collectCfg, dataClient, cache, collectObserver, met, logger)

	// Stable-favorite strategy — separate from whale-flow detection.
	// Runs only when Postgres is wired (it reads from polymarket_*) AND
	// the operator opts in via STABLE_FAVORITE_ENABLED=true.
	var stableFavWorker *stablefavorite.Worker
	if cfg.Postgres.Enabled() && cfg.StableFavorite.Enabled {
		sfCfg := sfdet.Config{
			Enabled:                    cfg.StableFavorite.Enabled,
			MinLifecyclePct:            cfg.StableFavorite.MinLifecyclePct,
			HotLifecyclePct:            cfg.StableFavorite.HotLifecyclePct,
			MinProbability:             cfg.StableFavorite.MinProbability,
			MaxProbability:             cfg.StableFavorite.MaxProbability,
			MinReturnPct:               cfg.StableFavorite.MinReturnPct,
			StabilityWindow:            cfg.StableFavorite.StabilityWindow,
			MaxPriceStddev:             cfg.StableFavorite.MaxPriceStddev,
			MaxDrawdown:                cfg.StableFavorite.MaxDrawdown,
			MaxAdverseMove6h:           cfg.StableFavorite.MaxAdverseMove6h,
			MaxNegativeDrift6h:         cfg.StableFavorite.MaxNegativeDrift6h,
			MinMarketVolumeUSD:         cfg.StableFavorite.MinMarketVolumeUSD,
			MinRecentTrades:            cfg.StableFavorite.MinRecentTrades,
			CrossMarketEnabled:         cfg.StableFavorite.CrossMarketEnabled,
			MaxCrossMarketDisagreement: cfg.StableFavorite.MaxCrossMarketDisagreement,
		}
		sfDet := sfdet.New(sfCfg)
		sfCache := stableFavoriteCacheAdapter{cache: cache}
		stableFavWorker = stablefavorite.New(stablefavorite.Config{
			Enabled:         true,
			Interval:        cfg.StableFavorite.Interval,
			CandidateLimit:  int32(cfg.StableFavorite.CandidateLimit),
			StrategyVersion: anomaly.StrategyIdentity,
		}, sfDet, tradesRepo, tradesRepo, sfCache, nil, alertsRepo, emitter, met, logger)
	}

	httpSrv := httpsrv.New(cfg.Application.MetricsPort, met.Registry(), logger)

	// AI analysis service. The Analyzer is either:
	//   - openai.Client when AIAnalysis.Enabled=true AND a key is set
	//   - analysis.NoopAnalyzer otherwise (service works without AI).
	// The aianalysis.Service wraps either Analyzer and adds the
	// refresh/cost policy + persistence. Wiring stays inert when
	// alertAnalysisRepo is nil (memory-only mode).
	var aiSvc *aianalysis.Service
	var eventPageProvider *eventpagecontext.Provider
	var catalystProvider *eventcatalyst.Provider
	if alertAnalysisRepo != nil {
		var analyzer analysis.Analyzer = analysis.NoopAnalyzer{}
		// Startup classification — answers the operator's first
		// question after a deploy ("is AI on or off, and why?").
		switch {
		case !cfg.AIAnalysis.Enabled:
			logger.Info().
				Str("reason", "AI_ANALYSIS_ENABLED=false").
				Bool("telegram_alerts_enabled", cfg.AIAnalysis.AlertsEnabled).
				Bool("reports_enabled", cfg.AIAnalysis.ReportsEnabled).
				Msg("ai analysis disabled")
		case cfg.AIAnalysis.APIKey == "":
			logger.Warn().
				Str("model", cfg.AIAnalysis.Model).
				Bool("telegram_alerts_enabled", cfg.AIAnalysis.AlertsEnabled).
				Msg("ai analysis configured but api key missing — falling back to NoopAnalyzer")
		default:
			analyzer = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.AIAnalysis.Timeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
				WebSearchEnabled:       cfg.AIAnalysis.WebSearchEnabled,
			})
			logger.Info().
				Str("provider", "openai").
				Str("model", cfg.AIAnalysis.Model).
				Dur("timeout", cfg.AIAnalysis.Timeout).
				Float64("daily_budget_usd", cfg.AIAnalysis.DailyBudgetUSD).
				Int("rate_limit_per_min", cfg.AIAnalysis.RateLimitPerMin).
				Bool("telegram_alerts_enabled", cfg.AIAnalysis.AlertsEnabled).
				Bool("reports_enabled", cfg.AIAnalysis.ReportsEnabled).
				Bool("market_intelligence_enabled", cfg.AIAnalysis.MarketIntelligenceEnabled).
				Bool("web_search_enabled", cfg.AIAnalysis.WebSearchEnabled).
				Msg("ai analysis enabled")
		}
		aiSvc = aianalysis.New(aianalysis.Config{
			AlertsEnabled:            cfg.AIAnalysis.Enabled && cfg.AIAnalysis.AlertsEnabled,
			LifecycleRefreshDeltaPct: cfg.AIAnalysis.LifecycleRefreshDeltaPct,
			CLVMaterialChange:        cfg.AIAnalysis.CLVMaterialChange,
		}, analyzer, alertAnalysisRepo,
			repository.NewAIRequestLogRepository(pgPool),
			met, logger)

		// Polymarket event-page narrative context. Resolves event_slug
		// from the Finding's market via marketsRepo.GetByConditionID,
		// fetches /event/<slug>.json (with buildId resolved + cached),
		// persists annotations + per-market snapshot, and renders a
		// compact prompt slot. Failure is silent: the loader returns
		// "" and aianalysis renders an "unavailable" slot. NEVER
		// blocks alert delivery.
		eventPageProvider = wireEventPageProvider(cfg, marketsRepo, met, logger)
		if eventPageProvider != nil {
			aiSvc.SetNarrativeLoader(eventPageProvider)
			if senderWorker != nil {
				senderWorker.SetAlertAnnotationStamper(eventPageProvider)
			}
			logger.Info().
				Bool("enabled", cfg.EventPage.Enabled).
				Dur("refresh_info", cfg.EventPage.RefreshInfo).
				Dur("refresh_important", cfg.EventPage.RefreshImportant).
				Dur("build_id_ttl", cfg.EventPage.BuildIDTTL).
				Int("prompt_max_items", cfg.EventPage.PromptMaxItems).
				Msg("event page context: wired")
		}

		// Political-Catalyst Intelligence overlay. Reads
		// polymarket_event_catalysts via the same conditionID →
		// event_slug resolver used by the event-page provider.
		// Failure NEVER blocks the alert path.
		catalystProvider = wireCatalystProvider(cfg, marketsRepo, met, logger)
		if catalystProvider != nil {
			aiSvc.SetCatalystLoader(catalystProvider)
			if senderWorker != nil {
				senderWorker.SetBlockedAlertStamper(catalystProvider)
			}
			logger.Info().
				Bool("enabled", cfg.Catalyst.Enabled).
				Int("prompt_max_items", cfg.Catalyst.PromptMaxItems).
				Msg("event catalyst: wired")
		}

		// Wire the AI enricher into the sender so every claimed alert
		// generates / refreshes its analyst note BEFORE Telegram render.
		// The sender already handles enricher==nil; this is the prod
		// hookup. When AlertsEnabled=false the service short-circuits
		// with a Status=skipped row so the alertsender logs `reason=disabled`.
		if senderWorker != nil {
			senderWorker.SetAIEnricher(aiSvc)
			logger.Info().
				Bool("ai_telegram_alerts_enabled", cfg.AIAnalysis.Enabled && cfg.AIAnalysis.AlertsEnabled).
				Msg("alertsender: ai enricher wired")
		}
	} else {
		logger.Warn().Msg("ai analysis disabled (postgres not configured — alert analyses cannot be persisted)")
	}

	// Strategy attribution writer: every alert that reaches the
	// sender lands a bucketed row on polymarket_alert_strategy_dimensions
	// so the "which-setups-actually-win" dashboards can group by
	// strategy_family / lifecycle / odds / notional / category without
	// recomputing buckets from raw payloads. Postgres-only (the table
	// lives there); failures degrade silently inside the sender.
	if cfg.Postgres.Enabled() && senderWorker != nil {
		senderWorker.SetAttributionStore(repository.NewStrategyDimensionsRepository(pgPool))
	}

	// Outcome-learning worker: scans resolved alerts, calls AI for
	// postmortems, edits original Telegram message + applies the
	// success/failure reaction. Runs only when Postgres + Telegram
	// + the alerts repo are wired; the AI key is OPTIONAL — without
	// it the NoopAnalyzer returns StatusSkipped and the worker
	// still applies reactions + sends a "Resolution" follow-up so
	// the operator sees the result.
	var outcomeAIWorker *outcomeai.Worker
	if cfg.Postgres.Enabled() && alertsRepo != nil && bot != nil && cfg.AIAnalysis.Enabled {
		var analyzerForOutcome analysis.Analyzer = analysis.NoopAnalyzer{}
		if cfg.AIAnalysis.APIKey != "" {
			analyzerForOutcome = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.AIAnalysis.Timeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
		}
		outcomeAIWorker = outcomeai.New(outcomeai.Config{
			Enabled:         cfg.AIAnalysis.ReportsEnabled,
			Interval:        10 * time.Minute,
			ClaimLimit:      16,
			ChatID:          cfg.Alerting.TelegramChatID,
			SuccessReaction: cfg.TelegramReactions.SuccessEmoji,
			FailureReaction: cfg.TelegramReactions.FailureEmoji,
		}, alertsRepo, repository.NewAlertOutcomeAnalysisRepository(pgPool), analyzerForOutcome, bot, logger)
		if eventPageProvider != nil {
			outcomeAIWorker.SetNarrativeLoader(eventPageProvider)
		}
	}

	// 2h market intelligence report worker. Same gating: Postgres
	// + Telegram wired AND AIMarketIntelligenceEnabled=true. The
	// candidate selection works even without the AI key — the
	// worker stores a "skipped" row and posts the candidate list
	// unranked. With the key, the AI summary lands inline.
	var marketIntelWorker *marketintel.Worker
	if cfg.Postgres.Enabled() && bot != nil && cfg.AIAnalysis.MarketIntelligenceEnabled {
		var analyzerForReport analysis.Analyzer = analysis.NoopAnalyzer{}
		if cfg.AIAnalysis.APIKey != "" {
			analyzerForReport = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.AIAnalysis.Timeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MarketIntelligenceMaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
				// Market intel prompt asks the model to scan fresh
				// news for trend confirmation/invalidation; without
				// web_search the section is dead.
				WebSearchEnabled: cfg.AIAnalysis.WebSearchEnabled,
			})
		}
		intelRepo := repository.NewMarketIntelligenceRepository(pgPool)
		marketIntelWorker = marketintel.New(marketintel.Config{
			Enabled:        true,
			Interval:       cfg.AIAnalysis.MarketIntelligenceInterval,
			MaxMarkets:     cfg.AIAnalysis.MarketIntelligenceMaxMarkets,
			MaxOutputChars: cfg.AIAnalysis.MarketIntelligenceMaxOutputChars,
			ChatID:         cfg.Alerting.TelegramChatID,
		}, intelRepo, intelRepo, analyzerForReport, bot, logger)
		marketIntelWorker.SetMetrics(met)
		if eventPageProvider != nil {
			marketIntelWorker.SetNarrativeLoader(eventPageProvider)
		}
		// v9.7: annotation ranking hook. When the AI key is wired,
		// the marketintel report appends a "Top important
		// annotations" block ranked by the model.
		if cfg.AIAnalysis.APIKey != "" && eventPageProvider != nil {
			ranker := openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.AIAnalysis.Timeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
			annoRepoForHook := repository.NewAnnotationIntelRepository(pgPool)
			hook := annotationranking.New(annotationranking.Config{},
				marketsRepo, eventPageProvider, ranker, annoRepoForHook, met, logger)
			marketIntelWorker.SetAnnotationRankingHook(hook)
		}
	}

	// v9.6 Political-Catalyst Intelligence importer. Every Interval
	// (default 5m) the importer refreshes annotations + extracts
	// catalysts via AI + upserts them. Operator seeding is no longer
	// required. Disabled in dev (no Postgres / no event-page
	// provider) and when EVENT_CATALYST_IMPORTER_ENABLED=false.
	var catalystImporterWorker *catalystimporter.Worker
	if cfg.Postgres.Enabled() && cfg.Catalyst.ImporterEnabled && marketsRepo != nil && eventPageProvider != nil {
		// Use a dedicated openai.Client instance for catalyst
		// extraction so it doesn't share the rate-bucket / budget
		// ledger with the per-alert analyzer. When the API key is
		// missing, the importer falls back to NoopExtractor — it
		// still refreshes annotations but emits no catalysts.
		var extractor analysis.CatalystExtractor = analysis.NoopExtractor{}
		if cfg.AIAnalysis.APIKey != "" && cfg.Catalyst.ImporterAIEnabled {
			extractor = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.Catalyst.ImporterAITimeout,
				MaxPromptChars:         cfg.Catalyst.ImporterMaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
				// The extractor uses JSON mode + strict-JSON parse;
				// web_search is not used for this path.
			})
		}
		intelRepo := repository.NewMarketIntelligenceRepository(pgPool)
		catalystRepo := repository.NewEventCatalystRepository(pgPool)
		catalystImporterWorker = catalystimporter.New(catalystimporter.Config{
			Enabled:              cfg.Catalyst.ImporterEnabled,
			Interval:             cfg.Catalyst.ImporterInterval,
			CategoryWhitelist:    splitCSV(cfg.Catalyst.ImporterCategoryCSV),
			BatchSize:            cfg.Catalyst.ImporterBatchSize,
			Concurrency:          cfg.Catalyst.ImporterConcurrency,
			Lookback:             cfg.Catalyst.ImporterLookback,
			AIEnabled:            cfg.Catalyst.ImporterAIEnabled,
			AITimeout:            cfg.Catalyst.ImporterAITimeout,
			MaxAnnotations:       cfg.Catalyst.ImporterMaxAnnotations,
			MaxPromptChars:       cfg.Catalyst.ImporterMaxPromptChars,
			MinConfidence:        cfg.Catalyst.ImporterMinConfidence,
			StaleAfter:           cfg.Catalyst.ImporterStaleAfter,
			TieringEnabled:       cfg.Catalyst.ImporterTieringEnabled,
			Tier1Interval:        cfg.Catalyst.ImporterTier1Interval,
			Tier2Interval:        cfg.Catalyst.ImporterTier2Interval,
			Tier3Interval:        cfg.Catalyst.ImporterTier3Interval,
			Tier1MinVolume24hUSD: cfg.Catalyst.ImporterTier1MinVolume24hUSD,
			Tier1MinAlerts24h:    cfg.Catalyst.ImporterTier1MinAlerts24h,
			Tier2MinVolume24hUSD: cfg.Catalyst.ImporterTier2MinVolume24hUSD,
			Tier1Categories:      splitCSV(cfg.Catalyst.ImporterTier1CategoriesCSV),
		}, intelRepo, marketsRepo, eventPageProvider, catalystRepo, extractor, met, logger)
		logger.Info().
			Bool("enabled", cfg.Catalyst.ImporterEnabled).
			Dur("interval", cfg.Catalyst.ImporterInterval).
			Strs("category_whitelist", splitCSV(cfg.Catalyst.ImporterCategoryCSV)).
			Int("batch_size", cfg.Catalyst.ImporterBatchSize).
			Int("concurrency", cfg.Catalyst.ImporterConcurrency).
			Bool("ai_enabled", cfg.Catalyst.ImporterAIEnabled).
			Msg("event catalyst importer: wired")
	}

	// v9.7 Daily Political Intelligence report worker. Runs once per
	// day at DAILY_POLITICAL_INTEL_TIME in DAILY_POLITICAL_INTEL_TIMEZONE,
	// selects 100 markets, calls AI, splits + sends Telegram.
	var dailyIntelWorker *dailypoliticalintel.Worker
	if cfg.Postgres.Enabled() && cfg.DailyIntel.Enabled && marketsRepo != nil && eventPageProvider != nil {
		var dailyGen analysis.DailyPoliticalIntelGenerator = analysis.NoopDailyPoliticalIntelGenerator{}
		if cfg.AIAnalysis.APIKey != "" && cfg.DailyIntel.AIEnabled {
			dailyGen = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.DailyIntel.AITimeout,
				MaxPromptChars:         cfg.DailyIntel.PromptMaxChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
		}
		intelRepo := repository.NewMarketIntelligenceRepository(pgPool)
		catalystRepo := repository.NewEventCatalystRepository(pgPool)
		annoRepo := repository.NewAnnotationIntelRepository(pgPool)
		var tgAdapter dailypoliticalintel.Telegram
		if bot != nil {
			tgAdapter = dailyIntelTelegramAdapter{bot: bot}
		}
		dailyIntelWorker = dailypoliticalintel.New(dailypoliticalintel.Config{
			Enabled:              cfg.DailyIntel.Enabled,
			TimeOfDay:            cfg.DailyIntel.TimeOfDay,
			Timezone:             cfg.DailyIntel.Timezone,
			MarketLimit:          cfg.DailyIntel.MarketLimit,
			AnnotationsPerMarket: cfg.DailyIntel.AnnotationsPerMarket,
			AIEnabled:            cfg.DailyIntel.AIEnabled,
			AITimeout:            cfg.DailyIntel.AITimeout,
			PromptMaxChars:       cfg.DailyIntel.PromptMaxChars,
			SendTelegram:         cfg.DailyIntel.SendTelegram,
			ChatID:               cfg.Alerting.TelegramChatID,
		}, intelRepo, marketsRepo, eventPageProvider, catalystRepo, annoRepo, dailyGen, tgAdapter, met, logger)
		// v9.8: wire the deterministic flow aggregator so the daily
		// AI sees real Watchtower context instead of empty fields.
		if cfg.EventFlow.Enabled {
			flowRepo := eventflow.New(sqlc.New(pgPool), eventflow.Config{
				Enabled:          true,
				Lookback:         cfg.EventFlow.Lookback,
				MaxAlerts:        cfg.EventFlow.MaxAlerts,
				MaxTrades:        cfg.EventFlow.MaxTrades,
				MinLargeTradeUSD: cfg.EventFlow.MinLargeTradeUSD,
				TopItems:         cfg.EventFlow.TopItems,
			}, met, logger)
			dailyIntelWorker.SetFlowLoader(flowRepo)
		}
		logger.Info().
			Bool("enabled", cfg.DailyIntel.Enabled).
			Str("time_of_day", cfg.DailyIntel.TimeOfDay).
			Str("timezone", cfg.DailyIntel.Timezone).
			Int("market_limit", cfg.DailyIntel.MarketLimit).
			Bool("ai_enabled", cfg.DailyIntel.AIEnabled).
			Bool("send_telegram", cfg.DailyIntel.SendTelegram).
			Msg("daily political intel: wired")
	}

	// v9.9 Prediction Evolution Worker (the heartbeat).
	var evolutionWorker *evolution.Worker
	if cfg.Postgres.Enabled() && cfg.Prediction.EvolutionEnabled && marketsRepo != nil && eventPageProvider != nil {
		predsRepo := repository.NewRepricingPredictionsRepository(pgPool)
		flowRepo := eventflow.New(sqlc.New(pgPool), eventflow.Config{
			Enabled:          true,
			Lookback:         cfg.EventFlow.Lookback,
			MaxAlerts:        cfg.EventFlow.MaxAlerts,
			MaxTrades:        cfg.EventFlow.MaxTrades,
			MinLargeTradeUSD: cfg.EventFlow.MinLargeTradeUSD,
			TopItems:         cfg.EventFlow.TopItems,
		}, met, logger)
		repricingComp := repricing.New(repricing.Config{
			Enabled:                cfg.Repricing.Enabled,
			Lookback:               cfg.Repricing.Lookback,
			PreWindow:              cfg.Repricing.PreWindow,
			PostWindow:             cfg.Repricing.PostWindow,
			MinAnnotationMove:      cfg.Repricing.MinAnnotationMove,
			MinFlowUSD:             cfg.Repricing.MinFlowUSD,
			UnderreactionThreshold: cfg.Repricing.UnderreactionThreshold,
			OverreactionThreshold:  cfg.Repricing.OverreactionThreshold,
		}, sqlc.New(pgPool), predsRepo, met, logger)
		var aiGen analysis.PredictionEvolutionGenerator = analysis.NoopPredictionEvolutionGenerator{}
		if cfg.AIAnalysis.APIKey != "" && cfg.Prediction.EvolutionAIEnabled {
			aiGen = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.Prediction.EvolutionTimeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
		}
		catalystRepoForEvolver := repository.NewEventCatalystRepository(pgPool)
		var tgAdapter evolution.Telegram
		if bot != nil && cfg.Prediction.EvolutionSendTelegram {
			tgAdapter = evolutionTelegramAdapter{bot: bot}
		}
		evolutionWorker = evolution.New(evolution.Config{
			Enabled:            cfg.Prediction.EvolutionEnabled,
			Interval:           cfg.Prediction.EvolutionInterval,
			BatchSize:          cfg.Prediction.EvolutionBatchSize,
			Concurrency:        cfg.Prediction.EvolutionConcurrency,
			Timeout:            cfg.Prediction.EvolutionTimeout,
			AIEnabled:          cfg.Prediction.EvolutionAIEnabled,
			AIMinInterval:      cfg.Prediction.EvolutionAIMinInterval,
			AIMaxPerRun:        cfg.Prediction.EvolutionAIMaxPerRun,
			StaleAfter:         cfg.Prediction.EvolutionStaleAfter,
			DecayEnabled:       cfg.Prediction.EvolutionDecayEnabled,
			DecayPerDay:        cfg.Prediction.EvolutionDecayPerDay,
			MinConfidence:      cfg.Prediction.EvolutionMinConfidence,
			MajorPriceMove:     cfg.Prediction.EvolutionMajorPriceMove,
			CatalystNearWindow: cfg.Prediction.EvolutionCatalystNearWindow,
			SendTelegram:       cfg.Prediction.EvolutionSendTelegram,
			TelegramCooldown:   cfg.Prediction.EvolutionTelegramCooldown,
			TelegramChatID:     cfg.Alerting.TelegramChatID,
		}, predsRepo, eventPageProvider, catalystRepoForEvolver, flowRepo, repricingComp, aiGen, tgAdapter, met, logger)
		logger.Info().
			Bool("enabled", cfg.Prediction.EvolutionEnabled).
			Dur("interval", cfg.Prediction.EvolutionInterval).
			Int("batch_size", cfg.Prediction.EvolutionBatchSize).
			Int("concurrency", cfg.Prediction.EvolutionConcurrency).
			Bool("ai_enabled", cfg.Prediction.EvolutionAIEnabled).
			Dur("ai_min_interval", cfg.Prediction.EvolutionAIMinInterval).
			Bool("decay_enabled", cfg.Prediction.EvolutionDecayEnabled).
			Bool("send_telegram", cfg.Prediction.EvolutionSendTelegram).
			Msg("prediction evolution: wired")
	}

	// AI budget governor — single process-local instance shared by
	// every worker that issues AI calls. nil-friendly seam: any
	// worker without a budget passed in fails open.
	aiBudget := aibudget.New(aibudget.Config{
		GlobalDailyBudgetUSD: cfg.AIBudget.GlobalDailyUSD,
		BucketDailyBudgetsUSD: map[string]float64{
			aibudget.BucketAlertAnalysis:     cfg.AIBudget.AlertAnalysisDailyUSD,
			aibudget.BucketCatalystImporter:  cfg.AIBudget.CatalystImporterDailyUSD,
			aibudget.BucketPredictionCreate:  cfg.AIBudget.PredictionCreationDailyUSD,
			aibudget.BucketPredictionEvolve:  cfg.AIBudget.PredictionEvolveDailyUSD,
			aibudget.BucketMarketIntel:       cfg.AIBudget.MarketIntelDailyUSD,
			aibudget.BucketDailyIntel:        cfg.AIBudget.DailyIntelDailyUSD,
			aibudget.BucketAnnotationRanking: cfg.AIBudget.AnnotationRankDailyUSD,
		},
	}, met)
	if evolutionWorker != nil {
		evolutionWorker.SetBudget(aiBudget)
	}
	if catalystImporterWorker != nil {
		catalystImporterWorker.SetBudget(aiBudget)
	}
	// v10.3: ensure every periodic AI surface is gated by the
	// shared aibudget governor. The alert analyzer is covered by
	// the openai.Client's internal ledger (AI_ANALYSIS_DAILY_BUDGET_USD);
	// these two were previously bypassing the v10.0 governor.
	if dailyIntelWorker != nil {
		dailyIntelWorker.SetBudget(aiBudget)
	}
	if marketIntelWorker != nil {
		marketIntelWorker.SetBudget(aiBudget)
	}
	// v10.2: usefulness scoring is persisted via the new
	// PredictionIntelligenceRepository. The evolver writes a fresh
	// score row at the end of every per-prediction cycle.
	if evolutionWorker != nil && cfg.Postgres.Enabled() {
		evolutionWorker.SetUsefulness(repository.NewPredictionIntelligenceRepository(pgPool))
	}

	// v10.2 prediction feedback worker.
	var predictionFeedback *predictionfeedback.Worker
	if cfg.Postgres.Enabled() && cfg.Prediction.FeedbackEnabled && marketsRepo != nil {
		horizons := parseHorizonsCSV(cfg.Prediction.FeedbackHorizonsCSV)
		intelRepo := repository.NewPredictionIntelligenceRepository(pgPool)
		eventPageRepo := repository.NewEventPageRepository(pgPool)
		predictionFeedback = predictionfeedback.New(
			predictionfeedback.Config{
				Enabled:           cfg.Prediction.FeedbackEnabled,
				Interval:          cfg.Prediction.FeedbackInterval,
				Horizons:          horizons,
				BatchSize:         cfg.Prediction.FeedbackBatchSize,
				MinMaterialDelta:  cfg.Prediction.EvaluationMinPriceDelta,
				UsefulEarlyWindow: cfg.Prediction.EvaluationUsefulEarlyWindow,
			},
			intelRepo, marketsRepo, eventPageRepo, tradesRepo, met, logger,
		)
		// v10.3: feedback worker also writes prediction evaluations.
		predictionFeedback.SetEvaluator(intelRepo)
		logger.Info().
			Bool("enabled", cfg.Prediction.FeedbackEnabled).
			Dur("interval", cfg.Prediction.FeedbackInterval).
			Int("horizons", len(horizons)).
			Msg("prediction feedback: wired")
	}

	// v10.3 prediction archival worker.
	var predictionArchiver *predictionarchival.Worker
	if cfg.Postgres.Enabled() && cfg.Prediction.ArchivalEnabled {
		intelRepo := repository.NewPredictionIntelligenceRepository(pgPool)
		predictionArchiver = predictionarchival.New(
			predictionarchival.Config{
				Enabled:            cfg.Prediction.ArchivalEnabled,
				Interval:           cfg.Prediction.ArchivalInterval,
				TerminalRetention:  cfg.Prediction.ArchivalTerminalRetention,
				StaleNoSignalAfter: cfg.Prediction.ArchivalStaleNoSignalAfter,
				BlockedRevalidate:  cfg.Prediction.ArchivalBlockedRevalidate,
				BatchSize:          cfg.Prediction.ArchivalBatchSize,
			},
			intelRepo, met, logger,
		)
		logger.Info().
			Bool("enabled", cfg.Prediction.ArchivalEnabled).
			Dur("interval", cfg.Prediction.ArchivalInterval).
			Dur("terminal_retention", cfg.Prediction.ArchivalTerminalRetention).
			Dur("stale_no_signal_after", cfg.Prediction.ArchivalStaleNoSignalAfter).
			Msg("prediction archival: wired")
	}

	// v10.0 Prediction Creation Worker (cold-start path). Without
	// this loop, the evolution worker has nothing to evolve.
	var predictionCreator *create.Worker
	if cfg.Postgres.Enabled() && cfg.Prediction.CreationEnabled && marketsRepo != nil && eventPageProvider != nil {
		predsRepo := repository.NewRepricingPredictionsRepository(pgPool)
		flowRepo := eventflow.New(sqlc.New(pgPool), eventflow.Config{
			Enabled:          true,
			Lookback:         cfg.EventFlow.Lookback,
			MaxAlerts:        cfg.EventFlow.MaxAlerts,
			MaxTrades:        cfg.EventFlow.MaxTrades,
			MinLargeTradeUSD: cfg.EventFlow.MinLargeTradeUSD,
			TopItems:         cfg.EventFlow.TopItems,
		}, met, logger)
		repricingComp := repricing.New(repricing.Config{
			Enabled:                cfg.Repricing.Enabled,
			Lookback:               cfg.Repricing.Lookback,
			PreWindow:              cfg.Repricing.PreWindow,
			PostWindow:             cfg.Repricing.PostWindow,
			MinAnnotationMove:      cfg.Repricing.MinAnnotationMove,
			MinFlowUSD:             cfg.Repricing.MinFlowUSD,
			UnderreactionThreshold: cfg.Repricing.UnderreactionThreshold,
			OverreactionThreshold:  cfg.Repricing.OverreactionThreshold,
		}, sqlc.New(pgPool), predsRepo, met, logger)
		catalystRepoForCreator := repository.NewEventCatalystRepository(pgPool)
		intelRepo := repository.NewMarketIntelligenceRepository(pgPool)
		var ranker analysis.PredictionRanker = analysis.NoopPredictionRanker{}
		var creator analysis.PredictionCreator = analysis.NoopPredictionCreator{}
		if cfg.AIAnalysis.APIKey != "" && cfg.Prediction.CreationAIEnabled {
			cli := openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.Prediction.CreationAIModel,
				Timeout:                cfg.Prediction.CreationAITimeout,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
			ranker = cli
			creator = cli
		}
		var creatorTG create.Telegram
		if bot != nil && cfg.Prediction.CreationSendTelegram {
			creatorTG = creationTelegramAdapter{bot: bot, chatID: cfg.Alerting.TelegramChatID}
		}
		predictionCreator = create.New(create.Config{
			Enabled:      cfg.Prediction.CreationEnabled,
			Interval:     cfg.Prediction.CreationInterval,
			BatchSize:    cfg.Prediction.CreationBatchSize,
			MaxSelected:  cfg.Prediction.CreationMaxSelected,
			MinScore:     cfg.Prediction.CreationMinScore,
			MaxPerDay:    cfg.Prediction.CreationMaxPerDay,
			DedupeWindow: cfg.Prediction.CreationDedupeWindow,
			AIEnabled:    cfg.Prediction.CreationAIEnabled,
			AITimeout:    cfg.Prediction.CreationAITimeout,
			SendTelegram: cfg.Prediction.CreationSendTelegram,
			Concurrency:  cfg.Prediction.CreationConcurrency,
			Categories:   cfg.Prediction.CreationCategories,
			// v10.1 Telegram polish + quality gate + throttling.
			AnnotationsEnabled:        cfg.Prediction.TelegramAnnotationsEnabled,
			AnnotationsLimit:          cfg.Prediction.TelegramAnnotationsLimit,
			AnnotationsMaxTitleChars:  cfg.Prediction.TelegramAnnotationsMaxTitleChars,
			AnnotationsMaxSourceNames: cfg.Prediction.TelegramAnnotationsMaxSourceNames,
			LinksEnabled:              cfg.Prediction.TelegramLinksEnabled,
			PolymarketBase:            cfg.Polymarket.PublicBaseURL,
			GrafanaBaseURL:            cfg.Alerting.GrafanaBaseURL,
			GrafanaDashUID:            cfg.Alerting.GrafanaDashUID,
			TelegramCooldown:          cfg.Prediction.CreationTelegramCooldown,
			MaxTelegramPerRun:         cfg.Prediction.CreationMaxTelegramPerRun,
			SendOnStartup:             cfg.Prediction.CreationSendOnStartup,
			SendNeutral:               cfg.Prediction.CreationSendNeutral,
			PersistLowQuality:         cfg.Prediction.CreationPersistLowQuality,
			MinConfidence:             cfg.Prediction.CreationMinConfidence,
			RequireSignal:             cfg.Prediction.CreationRequireSignal,
			MinSummaryChars:           cfg.Prediction.CreationMinSummaryChars,
		}, intelRepo, marketsRepo, predsRepo, eventPageProvider, catalystRepoForCreator, flowRepo, repricingComp, ranker, creator, creatorTG, met, logger)
		predictionCreator.SetBudget(aiBudget)
		logger.Info().
			Bool("enabled", cfg.Prediction.CreationEnabled).
			Dur("interval", cfg.Prediction.CreationInterval).
			Int("max_selected", cfg.Prediction.CreationMaxSelected).
			Int("max_per_day", cfg.Prediction.CreationMaxPerDay).
			Float64("min_score", cfg.Prediction.CreationMinScore).
			Bool("ai_enabled", cfg.Prediction.CreationAIEnabled).
			Bool("send_telegram", cfg.Prediction.CreationSendTelegram).
			Msg("prediction creation: wired")
	}

	return &App{
		cfg:               cfg,
		logger:            logger,
		metrics:           met,
		cache:             cache,
		discover:          discoverLoop,
		collect:           collectLoop,
		detectRun:         detectLoop.Run,
		httpSrv:           httpSrv,
		backfill:          backfillWorker,
		sender:            senderWorker,
		sanity:            sanityWorker,
		outcomes:          outcomesWorker,
		drift:             driftWorker,
		stats:             statsWorker,
		signalReport:      signalReportWorker,
		detection:         detectionWorker,
		stableFavorite:    stableFavWorker,
		aiAnalysis:        aiSvc,
		outcomeAI:         outcomeAIWorker,
		marketIntel:       marketIntelWorker,
		catalystImporter:  catalystImporterWorker,
		dailyIntel:        dailyIntelWorker,
		predictionEvolver: evolutionWorker,
		predictionCreator: predictionCreator,
		predictionFeedbk:  predictionFeedback,
		predictionArchive: predictionArchiver,
		pgPool:            pgPool,
	}, nil
}

// creationTelegramAdapter narrows *telegram.Bot to the create
// worker's small SendHTML seam. Keeps the create package free of a
// hard dependency on the telegram package.
type creationTelegramAdapter struct {
	bot    *telegram.Bot
	chatID string
}

func (a creationTelegramAdapter) SendHTML(ctx context.Context, body string) (int64, error) {
	res, err := a.bot.SendHTML(ctx, a.chatID, body)
	return res.MessageID, err
}

// evolutionTelegramAdapter narrows *telegram.Bot to the evolution
// worker's small Telegram seam — same pattern as the daily-intel
// adapter, kept local to keep cross-package wiring tight.
type evolutionTelegramAdapter struct{ bot *telegram.Bot }

func (a evolutionTelegramAdapter) SendHTML(ctx context.Context, chatID, text string) (evolution.TelegramResult, error) {
	res, err := a.bot.SendHTML(ctx, chatID, text)
	return evolution.TelegramResult{MessageID: res.MessageID}, err
}

// dailyIntelTelegramAdapter narrows *telegram.Bot to the small seam
// the daily-intel worker expects. We keep the seam local to the
// usecase package to avoid cross-importing infra/telegram.
type dailyIntelTelegramAdapter struct {
	bot *telegram.Bot
}

func (a dailyIntelTelegramAdapter) SendHTML(ctx context.Context, chatID, text string) (dailypoliticalintel.TelegramResult, error) {
	res, err := a.bot.SendHTML(ctx, chatID, text)
	return dailypoliticalintel.TelegramResult{MessageID: res.MessageID}, err
}

// parseHorizonsCSV converts "1h,6h,24h" into []time.Duration. Bad
// entries are silently dropped; an empty list falls back to the
// worker's default at construction time.
func parseHorizonsCSV(s string) []time.Duration {
	var out []time.Duration
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if d, err := time.ParseDuration(p); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// splitCSV trims and splits a comma-separated env value. Empty
// entries are dropped; the empty input returns nil.
func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// walletResolver returns a wallet-resolution callback for the
// detection worker. The worker has a trader_id but Observe expects
// trade.Taker to be a wallet address — this closure does the lookup.
//
// Failures map to empty string, which the worker reads as "trader
// axis disabled for this trade" — exactly like a wallet we've never
// seen. The repository call is a single indexed PK lookup so cost is
// negligible per trade.
func walletResolver(traders *repository.TraderRepository) detection.WalletResolver {
	if traders == nil {
		return nil
	}
	return func(ctx context.Context, traderID int64) string {
		wallet, err := traders.WalletByID(ctx, traderID)
		if err != nil {
			return ""
		}
		return wallet
	}
}

// telegramSenderAdapter narrows *telegram.Bot to statsreport.Sender by
// dropping the SendResult — the stats worker has no use for the
// upstream Telegram message id.
type telegramSenderAdapter struct{ bot *telegram.Bot }

func (a telegramSenderAdapter) SendHTML(ctx context.Context, chatID, text string) error {
	_, err := a.bot.SendHTML(ctx, chatID, text)
	return err
}

// telegramSignalSenderAdapter is the signalreport.Sender adapter. The
// signal-report worker DOES need the upstream message_id (it persists
// it on the polymarket_signal_reports row), so this adapter returns
// the int64 directly rather than dropping it.
type telegramSignalSenderAdapter struct{ bot *telegram.Bot }

func (a telegramSignalSenderAdapter) SendHTML(ctx context.Context, chatID, text string) (int64, error) {
	res, err := a.bot.SendHTML(ctx, chatID, text)
	if err != nil {
		return 0, err
	}
	return res.MessageID, nil
}

// reactionMetricsAdapter shims metrics.TelegramReactions over the
// outcomes.ReactionMetrics shape.
type reactionMetricsAdapter struct{ m *metrics.Metrics }

func (r reactionMetricsAdapter) ObserveReaction(status, reaction string) {
	if r.m == nil || r.m.TelegramReactions == nil {
		return
	}
	r.m.TelegramReactions.WithLabelValues(status, reaction).Inc()
}

// outcomeMetricsAdapter shims metrics.AlertOutcomes + the PAL family
// (realized edge / weighted success / calibration) over the
// outcomes.OutcomeMetrics interface. Methods tolerate nil receivers
// to keep tests cheap.
type outcomeMetricsAdapter struct{ m *metrics.Metrics }

func (o outcomeMetricsAdapter) ObserveOutcome(status, severity, kind string) {
	if o.m == nil || o.m.AlertOutcomes == nil {
		return
	}
	o.m.AlertOutcomes.WithLabelValues(status, severity, kind).Inc()
}

func (o outcomeMetricsAdapter) ObservePAL(snap outcomes.PALSnapshot) {
	if o.m == nil {
		return
	}
	// Calibration counter fires for every classified alert (including
	// pending — useful for "of all alerts in the 0-10% bucket, how
	// many resolved?"). The status label distinguishes contributions.
	if o.m.AlertCalibrationTotal != nil {
		o.m.AlertCalibrationTotal.
			WithLabelValues(snap.Bucket, string(snap.Status), snap.Severity, snap.Kind).
			Inc()
	}
	// Edge + weighted only when the verdict is resolved_correct or
	// resolved_wrong (snap.EdgeValid). Pending / unknown / unavailable
	// contribute only the calibration counter.
	if !snap.EdgeValid {
		return
	}
	if o.m.AlertRealizedEdge != nil {
		o.m.AlertRealizedEdge.WithLabelValues(snap.Severity, snap.Kind).Observe(snap.Edge)
	}
	if o.m.AlertWeightedResolvedTotal != nil && snap.Weight > 0 {
		o.m.AlertWeightedResolvedTotal.WithLabelValues(snap.Severity, snap.Kind).Add(snap.Weight)
	}
	if o.m.AlertWeightedSuccessTotal != nil && snap.Weight > 0 {
		o.m.AlertWeightedSuccessTotal.WithLabelValues(snap.Severity, snap.Kind).Add(snap.Weight * snap.SuccessBinary)
	}
}

// signalReportMetricsAdapter shims metrics.SignalReportsSent over the
// signalreport.Metrics shape.
type signalReportMetricsAdapter struct{ m *metrics.Metrics }

func (s signalReportMetricsAdapter) ObserveReportSent(periodType, status string) {
	if s.m == nil || s.m.SignalReportsSent == nil {
		return
	}
	s.m.SignalReportsSent.WithLabelValues(periodType, status).Inc()
}

// stableFavoriteCacheAdapter projects *marketcache.Cache into the
// MarketView the stablefavorite worker consumes. Kept here rather
// than in the worker package because *marketcache.Cache's return
// type (market.Market) lives in a separate domain package and would
// otherwise force the worker to depend on it transitively.
type stableFavoriteCacheAdapter struct{ cache *marketcache.Cache }

func (a stableFavoriteCacheAdapter) View(id vo.MarketID) (stablefavorite.MarketView, bool) {
	m, ok := a.cache.Get(id)
	if !ok {
		return stablefavorite.MarketView{}, false
	}
	return stablefavorite.MarketView{
		Tokens:   m.TokenIDs,
		Outcomes: m.Outcomes,
	}, true
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

// wireEventPageProvider constructs the Polymarket event-page
// narrative provider when enabled in config. Returns nil when the
// feature is disabled OR a market repo isn't available (memory mode
// or missing dependencies) — callers handle nil by skipping the
// loader wiring. Slug resolution is delegated to a closure over
// marketsRepo.GetByConditionID so the loader stays decoupled from
// the repository package.
func wireEventPageProvider(cfg *Config, marketsRepo *repository.MarketRepository, met *metrics.Metrics, logger *zerolog.Logger) *eventpagecontext.Provider {
	if !cfg.EventPage.Enabled || marketsRepo == nil {
		return nil
	}
	resolver := eventpage.NewBuildIDResolver(eventpage.BuildIDResolverConfig{
		HTMLBaseURL: cfg.EventPage.HTMLBaseURL,
		Timeout:     cfg.EventPage.FetchTimeout,
		TTL:         cfg.EventPage.BuildIDTTL,
		Logger:      logger,
	})
	client, err := eventpage.NewClient(eventpage.ClientConfig{
		HTMLBaseURL: cfg.EventPage.HTMLBaseURL,
		Resolver:    resolver,
		Logger:      logger,
	})
	if err != nil {
		logger.Warn().Err(err).Msg("event page client: construction failed; narrative context disabled")
		return nil
	}
	repo := repository.NewEventPageRepository(a_pgPool(marketsRepo))
	// The slug resolver closes over the market repo; a missing
	// market (purged, never seen) returns "" so the loader skips
	// the call cleanly.
	slugResolver := func(ctx context.Context, conditionID string) string {
		if conditionID == "" {
			return ""
		}
		m, err := marketsRepo.GetByConditionID(ctx, conditionID)
		if err != nil {
			return ""
		}
		return m.EventSlug
	}
	return eventpagecontext.New(eventpagecontext.Config{
		Enabled:          cfg.EventPage.Enabled,
		RefreshInfo:      cfg.EventPage.RefreshInfo,
		RefreshImportant: cfg.EventPage.RefreshImportant,
		PromptMaxItems:   cfg.EventPage.PromptMaxItems,
		PromptMaxChars:   cfg.EventPage.PromptMaxChars,
		FetchTimeout:     cfg.EventPage.FetchTimeout,
	}, client, repo, slugResolver, met, logger)
}

// a_pgPool is a tiny escape hatch: the event-page repo needs a
// *pgxpool.Pool, but we only thread *MarketRepository through the
// signature for slug resolution. The market repo carries the pool
// internally; we expose it here to avoid widening the wireEventPageProvider
// signature. Returning nil is safe — the repo handles a nil pool by
// failing every call, which the provider treats as a silent skip.
func a_pgPool(r *repository.MarketRepository) *pgxpool.Pool {
	return r.Pool()
}

// wireCatalystProvider constructs the Political-Catalyst Intelligence
// loader. Returns nil when the feature is disabled OR a market repo
// isn't available (memory mode / missing deps). Reuses the same
// conditionID → event_slug resolver as the event-page provider so
// the alert pipeline only has one slug-resolution semantic.
func wireCatalystProvider(cfg *Config, marketsRepo *repository.MarketRepository, met *metrics.Metrics, logger *zerolog.Logger) *eventcatalyst.Provider {
	if !cfg.Catalyst.Enabled || marketsRepo == nil {
		return nil
	}
	repo := repository.NewEventCatalystRepository(a_pgPool(marketsRepo))
	slugResolver := func(ctx context.Context, conditionID string) string {
		if conditionID == "" {
			return ""
		}
		m, err := marketsRepo.GetByConditionID(ctx, conditionID)
		if err != nil {
			return ""
		}
		return m.EventSlug
	}
	return eventcatalyst.New(eventcatalyst.Config{
		Enabled:        cfg.Catalyst.Enabled,
		PromptMaxItems: cfg.Catalyst.PromptMaxItems,
		PromptMaxChars: cfg.Catalyst.PromptMaxChars,
	}, repo, slugResolver, met, logger)
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
	if a.signalReport != nil {
		execs = append(execs, shutdown2.Exec{Name: "signalreport", Fn: a.signalReport.Run})
	}
	if a.detection != nil {
		execs = append(execs, shutdown2.Exec{Name: "detection", Fn: func(ctx context.Context) error {
			a.detection.Run(ctx)
			return nil
		}})
	}
	if a.stableFavorite != nil {
		execs = append(execs, shutdown2.Exec{Name: "stablefavorite", Fn: func(ctx context.Context) error {
			a.stableFavorite.Run(ctx)
			return nil
		}})
	}
	if a.outcomeAI != nil {
		execs = append(execs, shutdown2.Exec{Name: "outcomeai", Fn: func(ctx context.Context) error {
			a.outcomeAI.Run(ctx)
			return nil
		}})
	}
	if a.marketIntel != nil {
		execs = append(execs, shutdown2.Exec{Name: "marketintel", Fn: func(ctx context.Context) error {
			a.marketIntel.Run(ctx)
			return nil
		}})
	}
	if a.catalystImporter != nil {
		execs = append(execs, shutdown2.Exec{Name: "catalyst-importer", Fn: func(ctx context.Context) error {
			a.catalystImporter.Run(ctx)
			return nil
		}})
	}
	if a.dailyIntel != nil {
		execs = append(execs, shutdown2.Exec{Name: "daily-political-intel", Fn: func(ctx context.Context) error {
			a.dailyIntel.Run(ctx)
			return nil
		}})
	}
	if a.predictionEvolver != nil {
		execs = append(execs, shutdown2.Exec{Name: "prediction-evolution", Fn: func(ctx context.Context) error {
			a.predictionEvolver.Run(ctx)
			return nil
		}})
	}
	if a.predictionCreator != nil {
		execs = append(execs, shutdown2.Exec{Name: "prediction-creation", Fn: func(ctx context.Context) error {
			a.predictionCreator.Run(ctx)
			return nil
		}})
	}
	if a.predictionFeedbk != nil {
		execs = append(execs, shutdown2.Exec{Name: "prediction-feedback", Fn: func(ctx context.Context) error {
			a.predictionFeedbk.Run(ctx)
			return nil
		}})
	}
	if a.predictionArchive != nil {
		execs = append(execs, shutdown2.Exec{Name: "prediction-archival", Fn: func(ctx context.Context) error {
			a.predictionArchive.Run(ctx)
			return nil
		}})
	}

	return shutdown2.Graceful(
		ctx,
		execs,
		shutdown2.WithLogger(a.logger),
		shutdown2.WithFadeOutDuration(a.cfg.Application.ShutdownGracePeriod),
	)
}
