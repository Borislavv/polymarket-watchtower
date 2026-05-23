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
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/traderbaseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/backfill"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/concentration"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detection"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/discover"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/drift"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventcatalyst"
	catalystimporter "github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventcatalyst/importer"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketclosereview"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/newsintel"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomeai"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomes"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/persist"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/realtime"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/sanity"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/signalquality"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/stagedinputs"
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
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/ws"
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
	backfill          *backfill.Worker
	sender            *alertsender.Worker
	sanity            *sanity.Worker
	outcomes          *outcomes.Worker
	drift             *drift.Worker
	detection         *detection.Worker
	aiAnalysis        *aianalysis.Service
	outcomeAI         *outcomeai.Worker
	catalystImporter  *catalystimporter.Worker
	newsIntel         *newsintel.Worker
	signalQuality     *signalquality.Worker
	marketCloseReview *marketclosereview.Worker
	realtimeWS        *realtime.Worker

	// v11.6 Phase B workers (Postgres-only).
	strategyPhaseB StrategyPhaseB
	// v11.7 Phase C workers — 5 supporting workers + outcome backfill.
	strategyPhaseC StrategyPhaseC
	// v11.10 Phase F producers — real holdersync + bookbars against
	// verified public Polymarket endpoints.
	strategyPhaseF StrategyPhaseF

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
		pgPool            *pgxpool.Pool
		persistSink       *persist.Sink
		alertsRepo        *repository.AlertRepository
		alertAnalysisRepo *repository.AlertAnalysisRepository
		marketsRepo       *repository.MarketRepository
		tradesRepo        *repository.TradeRepository
		tradersRepo       *repository.TraderRepository
		dbBaseline        *dbbaseline.Provider
		traderBaseline    *traderbaseline.Provider
		mmFilter          *mmfilter.Filter
		backfillWorker    *backfill.Worker
		senderWorker      *alertsender.Worker
		sanityWorker      *sanity.Worker
		outcomesWorker    *outcomes.Worker
		driftWorker       *drift.Worker
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
	// guardedTG is the SendHTML-only sender that runs every outgoing
	// Telegram message through the central suppression guard. Real
	// alerts pass through transparently; messages that match a
	// disabled-surface marker (Watchtower stats / PREDICTION UPDATE /
	// state transition / blocked) are dropped + counted. Outcome-reactor
	// paths that call EditMessageText / SetMessageReaction continue to
	// use the raw `bot` handle.
	// guardedHTML is the low-level HTML transport wrapped by the
	// legacy text-marker guard (defense-in-depth tripwire for any
	// untyped call site).
	var guardedHTML telegram.HTMLSender
	// telegramRouter is the v11.3 typed Sender every new worker
	// targets. It resolves chat id from the message Surface so a
	// flow alert ALWAYS goes to the signal chat and a signal-quality
	// report ALWAYS goes to the admin chat — no matter what the
	// worker has been told.
	var telegramRouter *telegram.Router
	// telegramAnnotation is the v11.4 typed adapter for the non-Send
	// Telegram operations (EditMessageText / SetMessageReaction).
	// outcomeai + the v11.4 Market Close Review worker both use it.
	var telegramAnnotation *telegram.Annotation
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
		// v11.2: all noise surfaces removed at the worker layer; the
		// guard is now a defense-in-depth tripwire that catches any
		// future regression that re-introduces those bodies.
		guardCfg := telegram.GuardConfig{
			WatchtowerStatsEnabled:           false,
			PredictionUpdateEnabled:          false,
			PredictionStateTransitionEnabled: false,
			PredictionBlockedEnabled:         false,
			// v11.3: tell the guard which chat is the signal feed
			// so it can suppress admin-marker bodies that try to
			// reach customers via the wrong chat.
			SignalChatID: cfg.Alerting.TelegramChatID,
		}
		guardedHTML = telegram.NewGuard(bot, guardCfg, met.TelegramSuppressed)
		logger.Info().
			Bool("watchtower_stats", guardCfg.WatchtowerStatsEnabled).
			Bool("prediction_update", guardCfg.PredictionUpdateEnabled).
			Bool("prediction_state_transition", guardCfg.PredictionStateTransitionEnabled).
			Bool("prediction_blocked", guardCfg.PredictionBlockedEnabled).
			Msg("telegram guard: wired (v11.1)")

		// v11.3 typed router. Validate() fails loud at boot so a
		// misconfigured admin chat never silently swaps to the
		// signal chat.
		routerCfg := telegram.RouterConfig{
			SignalEnabled:             true,
			SignalChatID:              cfg.Alerting.TelegramChatID,
			AdminEnabled:              cfg.Alerting.TelegramAdminEnabled,
			AdminChatID:               cfg.Alerting.TelegramAdminChatID,
			AllowSameChat:             cfg.Alerting.TelegramAllowSameChatAdmin,
			AdminSignalQualityReports: cfg.Alerting.TelegramAdminSignalQualityReports,
			AdminStats:                cfg.Alerting.TelegramAdminStats,
			AdminStrategyScorecard:    cfg.Alerting.TelegramAdminStrategyScorecard,
			AdminOperationalHealth:    cfg.Alerting.TelegramAdminOperationalHealth,
			AdminBudgetReports:        cfg.Alerting.TelegramAdminBudgetReports,
			AdminSuppressionReports:   cfg.Alerting.TelegramAdminSuppressionReports,
		}
		if err := routerCfg.Validate(); err != nil {
			return nil, fmt.Errorf("telegram router config: %w", err)
		}
		routerMetrics := telegram.PromMetricsAdapter{
			Route:      met.TelegramRoute,
			Sent:       met.TelegramSent,
			Suppressed: met.TelegramSuppressedV2,
			SendFailed: met.TelegramSendFailed,
		}
		telegramRouter = telegram.NewRouter(routerCfg, guardedHTML, routerMetrics)
		telegramAnnotation = telegram.NewAnnotation(bot, telegram.PromAnnotationMetricsAdapter{
			Annotation:       met.TelegramAnnotation,
			AnnotationFailed: met.TelegramAnnotationFailed,
		})
		logger.Info().
			Str("signal_chat_id", cfg.Alerting.TelegramChatID).
			Bool("admin_enabled", routerCfg.AdminEnabled).
			Str("admin_chat_id", routerCfg.AdminChatID).
			Bool("admin_signal_quality_reports", routerCfg.AdminSignalQualityReports).
			Bool("admin_stats", routerCfg.AdminStats).
			Bool("admin_operational_health", routerCfg.AdminOperationalHealth).
			Msg("telegram router: wired (v11.3)")

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
		}, alertsRepo, telegramRouter, met, logger)
		logger.Info().
			Str("chat_id", cfg.Alerting.TelegramChatID).
			Int("workers", cfg.AlertSender.Workers).
			Msg("alertsender: enabled")

		// v11.2 cleanup: periodic stats Telegram heartbeat removed.
		// Operators read pipeline health from Grafana, not Telegram.

		// v11.2 cleanup: scheduled signal-quality Telegram reports
		// removed. Outcome / drift persistence remains; the
		// scheduled Telegram report worker is gone.
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
		// v10.8 concentration gate. Default-on; the operator can
		// disable by setting EVENT_ALERT_CONCENTRATION_LIMIT=0.
		if cfg.Anomaly.EventAlertConcentrationLimit > 0 {
			detectCfg.Concentration = concentration.New(concentration.Config{
				EventConcentrationLimit:          cfg.Anomaly.EventAlertConcentrationLimit,
				EventConcentrationWindow:         cfg.Anomaly.EventAlertConcentrationWindow,
				RepeatedEventThresholdMultiplier: cfg.Anomaly.RepeatedEventThresholdMultiplier,
				WalletAlertCooldown:              cfg.Anomaly.WalletAlertCooldown,
				AccumulationEscalationFactor:     cfg.Anomaly.AccumulationEscalationFactor,
				SeverityFloor:                    "info",
			})
			detectCfg.ConcentrationHistory = alertsRepo
		}
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

	// v11.6 Phase B: wire strategy bus + rulesrisk + value evaluator
	// + promotion review. All inert when StrategyConfig keeps every
	// strategy disabled (the default), but the bus is ALWAYS injected
	// when Postgres is wired so per-strategy operator flips take
	// effect without a binary restart.
	strategyPhaseB := wireStrategyPhaseB(pgPool, cfg.Strategy, met, logger)
	if strategyPhaseB.Bus != nil {
		detectCfg.StrategyShadowBus = strategyPhaseB.Bus
		detectCfg.StrategyRulesRisk = strategyPhaseB.RulesRisk
		detectCfg.StrategyShadowMaxPerTrade = cfg.Strategy.ShadowMaxDecisionsPerTrade
		detectCfg.StrategyShadowRecordNoFire = cfg.Strategy.ShadowRecordNoFire
		// v11.8: staged inputs bridge — hot-path readers backed by
		// Postgres + TTL cache. nil when Postgres absent.
		detectCfg.StrategyStagedInputs = stagedinputs.New(pgPool, stagedinputs.Config{
			Enabled:      cfg.Strategy.StagedInputs.Enabled,
			CacheEnabled: cfg.Strategy.StagedInputs.CacheEnabled,
			CacheTTL:     cfg.Strategy.StagedInputs.CacheTTL,
			MaxRows:      cfg.Strategy.StagedInputs.MaxRows,
			QueryTimeout: cfg.Strategy.StagedInputs.QueryTimeout,
		})
	}
	// v11.7 Phase C — production adapters for the 5 supporting
	// workers + outcome backfill evaluator. Inert when their
	// per-worker *_ENABLED flags stay false.
	strategyPhaseC := wireStrategyPhaseC(pgPool, cfg.Strategy, met, strategyPhaseB.RulesRisk)

	// v11.10 Phase F — real-source producers (holdersync via Data
	// API /holders, bookbars via CLOB /book + /books). When
	// HOLDERSYNC_SOURCE_MODE=dataapi we replace the v11.7 stub.
	strategyPhaseF, realHolderSync := wireStrategyPhaseF(pgPool, cfg.Strategy, met, dataClient)
	if realHolderSync != nil {
		strategyPhaseC.HolderSync = realHolderSync
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

	// v11.2 cleanup: stable-favorite strategy fully removed. Not
	// wired into detect.Loop and no downstream consumer remained.

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
		}, alertsRepo, repository.NewAlertOutcomeAnalysisRepository(pgPool), analyzerForOutcome, telegramRouter, telegramAnnotation, logger)
		if eventPageProvider != nil {
			outcomeAIWorker.SetNarrativeLoader(eventPageProvider)
		}
	}

	// 2h market intelligence report worker. Same gating: Postgres
	// + Telegram wired AND AIMarketIntelligenceEnabled=true. The
	// candidate selection works even without the AI key — the
	// worker stores a "skipped" row and ships a deterministic
	// fallback report with markets + annotation links. With the
	// key, the AI summary lands inline.
	// v11.2 cleanup: market intelligence + annotation ranking
	// surfaces fully removed. The 2h "Market intelligence" Telegram
	// report and the AI-ranked annotation appendix are gone; AI
	// budget for these buckets is no longer reserved.

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
	// v11.2 cleanup: daily political intelligence worker fully
	// removed. The once-per-day Telegram report is gone; the
	// dailypoliticalintel package is no longer wired.

	// v11.0 Hourly News Intelligence Worker — the v11.0 product.
	// One AI call per hour over NEW Polymarket annotations attached
	// to whitelisted markets. Replaces the v10.x prediction +
	// market-intel surfaces. Silent when no new news or no edge.
	var newsIntelWorker *newsintel.Worker
	if cfg.Postgres.Enabled() && cfg.AIAnalysis.NewsIntelEnabled {
		eventPageRepo := repository.NewEventPageRepository(pgPool)
		newsIntelRepo := repository.NewNewsIntelRepository(pgPool)
		var newsAnalyzer newsintel.Analyzer
		if cfg.AIAnalysis.NewsIntelAIEnabled && cfg.AIAnalysis.APIKey != "" {
			newsAnalyzer = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  cfg.AIAnalysis.Model,
				Timeout:                cfg.AIAnalysis.NewsIntelAITimeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
		}
		var newsTG newsintel.TelegramSender
		if telegramRouter != nil && cfg.AIAnalysis.NewsIntelSendTelegram {
			newsTG = telegramRouter
		}
		newsIntelWorker = newsintel.New(newsintel.Config{
			Enabled:            cfg.AIAnalysis.NewsIntelEnabled,
			StartupRun:         cfg.AIAnalysis.NewsIntelStartupRun,
			Interval:           cfg.AIAnalysis.NewsIntelInterval,
			Lookback:           cfg.AIAnalysis.NewsIntelLookback,
			MaxItems:           cfg.AIAnalysis.NewsIntelMaxItems,
			MaxMarketsPerItem:  cfg.AIAnalysis.NewsIntelMaxMarketsPerItem,
			MaxSelected:        cfg.AIAnalysis.NewsIntelMaxSelected,
			AIEnabled:          cfg.AIAnalysis.NewsIntelAIEnabled,
			AITimeout:          cfg.AIAnalysis.NewsIntelAITimeout,
			SendTelegram:       cfg.AIAnalysis.NewsIntelSendTelegram,
			SuppressNoEdge:     cfg.AIAnalysis.NewsIntelSuppressNoEdge,
			DedupeEnabled:      cfg.AIAnalysis.NewsIntelDedupeEnabled,
			SemanticCooldown:   cfg.AIAnalysis.NewsIntelSemanticCooldown,
			MinConfidence:      cfg.AIAnalysis.NewsIntelMinConfidence,
			ChatID:             cfg.Alerting.TelegramChatID,
			TelegramMessageCap: 3500,
		}, eventPageRepo, eventPageRepo, newsIntelRepo, newsAnalyzer, newsTG, met, logger)
		logger.Info().
			Bool("enabled", cfg.AIAnalysis.NewsIntelEnabled).
			Bool("startup_run", cfg.AIAnalysis.NewsIntelStartupRun).
			Dur("interval", cfg.AIAnalysis.NewsIntelInterval).
			Dur("lookback", cfg.AIAnalysis.NewsIntelLookback).
			Int("max_items", cfg.AIAnalysis.NewsIntelMaxItems).
			Int("max_selected", cfg.AIAnalysis.NewsIntelMaxSelected).
			Bool("ai_enabled", cfg.AIAnalysis.NewsIntelAIEnabled && cfg.AIAnalysis.APIKey != "").
			Bool("send_telegram", cfg.AIAnalysis.NewsIntelSendTelegram && bot != nil).
			Msg("news intelligence: wired (v11.0)")
	}

	// v11.3 Signal-quality admin telemetry worker.
	// Reads polymarket_alerts deterministically (no AI) and posts
	// a periodic "Signal quality · Daily ..." body to the admin
	// chat. The Router enforces destination — even if some future
	// code path passes the wrong chat id, the body never reaches
	// the customer signal feed.
	var signalQualityWorker *signalquality.Worker
	if cfg.Postgres.Enabled() &&
		telegramRouter != nil &&
		cfg.Alerting.TelegramAdminEnabled &&
		cfg.Alerting.TelegramAdminSignalQualityReports {
		signalQualityWorker = signalquality.New(
			signalquality.Config{
				Enabled:      true,
				Interval:     24 * time.Hour,
				StartupGrace: 5 * time.Minute,
				Period:       signalquality.PeriodDaily,
			},
			// v11.4 bounded store: daily worker reads the 24h
			// window, capped at SIGNAL_QUALITY_MAX_ALERTS rows.
			signalquality.NewStoreWithLimits(
				pgPool,
				cfg.Alerting.SignalQualityDailyLookback,
				cfg.Alerting.SignalQualityMaxAlerts,
			),
			telegramRouter,
			logger,
		)
		logger.Info().
			Str("admin_chat_id", cfg.Alerting.TelegramAdminChatID).
			Msg("signal-quality admin report: wired (v11.3)")
	}

	// v11.4 Market Close Review worker declared here, constructed
	// after aiBudget so the budget gate is wired in.
	var marketCloseReviewWorker *marketclosereview.Worker

	// v9.9 Prediction Evolution Worker (the heartbeat).
	// v11.2 cleanup: prediction evolution / feedback / archival
	// workers fully removed. The Telegram "PREDICTION UPDATE" surface
	// is gone and the underlying state machine is no longer wired.
	// `polymarket_market_predictions` and related tables remain in
	// the schema (append-only migration policy) but are no longer
	// written to.

	// AI budget governor — single process-local instance shared by
	// every worker that issues AI calls. nil-friendly seam: any
	// worker without a budget passed in fails open.
	aiBudget := aibudget.New(aibudget.Config{
		GlobalDailyBudgetUSD: cfg.AIBudget.GlobalDailyUSD,
		BucketDailyBudgetsUSD: map[string]float64{
			aibudget.BucketAlertAnalysis:     cfg.AIBudget.AlertAnalysisDailyUSD,
			aibudget.BucketCatalystImporter:  cfg.AIBudget.CatalystImporterDailyUSD,
			aibudget.BucketMarketCloseReview: cfg.Alerting.MarketCloseReviewDailyBudgetUSD,
		},
	}, met)
	if catalystImporterWorker != nil {
		catalystImporterWorker.SetBudget(aiBudget)
	}

	// v11.4 Market Close Review learning loop construction.
	// Reviews recently-closed markets, asks AI whether
	// Watchtower's alerts caught real informed flow, persists
	// verdict + cost, posts an admin Telegram summary, and
	// applies reactions to the original signal alerts.
	if cfg.Postgres.Enabled() &&
		cfg.Alerting.MarketCloseReviewEnabled &&
		telegramRouter != nil {
		var mcrAnalyzer marketclosereview.Analyzer
		if cfg.AIAnalysis.APIKey != "" && cfg.Alerting.MarketCloseReviewAIEnabled {
			model := cfg.Alerting.MarketCloseReviewAIModel
			if model == "" {
				model = cfg.AIAnalysis.Model
			}
			mcrAnalyzer = openai.New(openai.Config{
				APIKey:                 cfg.AIAnalysis.APIKey,
				BaseURL:                cfg.AIAnalysis.BaseURL,
				Model:                  model,
				Timeout:                cfg.Alerting.MarketCloseReviewAITimeout,
				MaxPromptChars:         cfg.AIAnalysis.MaxPromptChars,
				MaxOutputChars:         cfg.AIAnalysis.MaxOutputChars,
				RatePerMin:             cfg.AIAnalysis.RateLimitPerMin,
				DailyBudget:            cfg.AIAnalysis.DailyBudgetUSD,
				PromptCostPer1kUSD:     cfg.AIAnalysis.PromptCostPer1kUSD,
				CompletionCostPer1kUSD: cfg.AIAnalysis.CompletionCostPer1kUSD,
			})
		}
		marketCloseReviewWorker = marketclosereview.New(
			marketclosereview.Config{
				Enabled:                cfg.Alerting.MarketCloseReviewEnabled,
				Interval:               cfg.Alerting.MarketCloseReviewInterval,
				Lookback:               cfg.Alerting.MarketCloseReviewLookback,
				MarketMaxAgeAfterClose: cfg.Alerting.MarketCloseReviewMarketMaxAgeAfterClose,
				HistoryLookback:        cfg.Alerting.MarketCloseReviewHistoryLookback,
				MinAlerts:              cfg.Alerting.MarketCloseReviewMinAlerts,
				RequireAlertOrNews:     cfg.Alerting.MarketCloseReviewRequireAlertOrNews,
				MaxMarketsPerRun:       cfg.Alerting.MarketCloseReviewMaxMarketsPerRun,
				MaxAlertsPerMarket:     cfg.Alerting.MarketCloseReviewMaxAlertsPerMarket,
				MaxEventsPerMarket:     cfg.Alerting.MarketCloseReviewMaxEventsPerMarket,
				AIEnabled:              cfg.Alerting.MarketCloseReviewAIEnabled,
				AITimeout:              cfg.Alerting.MarketCloseReviewAITimeout,
				AIModel:                cfg.Alerting.MarketCloseReviewAIModel,
				DailyBudgetUSD:         cfg.Alerting.MarketCloseReviewDailyBudgetUSD,
				SendAdminTelegram:      cfg.Alerting.MarketCloseReviewSendAdminTelegram,
				SetReactions:           cfg.Alerting.MarketCloseReviewSetReactions,
				ReactionSuccess:        cfg.Alerting.MarketCloseReviewReactionSuccess,
				ReactionFailure:        cfg.Alerting.MarketCloseReviewReactionFailure,
				ReactionAmbiguous:      cfg.Alerting.MarketCloseReviewReactionAmbiguous,
				ReactionSkipAmbiguous:  cfg.Alerting.MarketCloseReviewReactionSkipAmbiguous,
				SignalChatID:           cfg.Alerting.TelegramChatID,
			},
			repository.NewMarketCloseReviewRepository(pgPool),
			mcrAnalyzer,
			aiBudget,
			telegramRouter,
			telegramAnnotation,
			met,
			logger,
		)
		logger.Info().
			Bool("enabled", cfg.Alerting.MarketCloseReviewEnabled).
			Dur("interval", cfg.Alerting.MarketCloseReviewInterval).
			Bool("ai_enabled", cfg.Alerting.MarketCloseReviewAIEnabled && cfg.AIAnalysis.APIKey != "").
			Float64("daily_budget_usd", cfg.Alerting.MarketCloseReviewDailyBudgetUSD).
			Msg("market close review: wired (v11.4)")
	}

	// v10.4 Hybrid WebSocket realtime fast-lane. WS_ENABLED=false
	// is the production default — the operator opts in per env.
	// When disabled the worker short-circuits without goroutines.
	var realtimeWorker *realtime.Worker
	if cfg.Postgres.Enabled() && cfg.WS.Enabled {
		realtimeStore := repository.NewRealtimeRepository(pgPool)
		wsClient := ws.New(ws.Config{
			Endpoint:     cfg.WS.Endpoint,
			MaxTokens:    cfg.WS.MaxTokens,
			PingInterval: cfg.WS.PingInterval,
			ReadTimeout:  cfg.WS.ReadTimeout,
			WriteTimeout: cfg.WS.WriteTimeout,
			ReconnectMin: cfg.WS.ReconnectMinBackoff,
			ReconnectMax: cfg.WS.ReconnectMaxBackoff,
			EventBuffer:  cfg.WS.EventBuffer,
		}, met, logger)
		selector := realtime.NewPostgresSelector(pgPool)
		selector.IncludeHighTrade = cfg.WS.IncludeHighTradeMarkets
		selector.HighTradeMinTrades24h = cfg.WS.HighTradeMinTrades24h
		selector.HighTradeLookbackHours = cfg.WS.HighTradeLookbackHours
		realtimeWorker = realtime.New(realtime.Config{
			Enabled:                   cfg.WS.Enabled,
			MarketStreamEnabled:       cfg.WS.MarketStreamEnabled,
			SubscriptionMode:          cfg.WS.SubscriptionMode,
			MaxMarkets:                cfg.WS.MaxMarkets,
			MaxTokens:                 cfg.WS.MaxTokens,
			ReconnectMin:              cfg.WS.ReconnectMinBackoff,
			ReconnectMax:              cfg.WS.ReconnectMaxBackoff,
			PingInterval:              cfg.WS.PingInterval,
			ReadTimeout:               cfg.WS.ReadTimeout,
			WriteTimeout:              cfg.WS.WriteTimeout,
			EventBuffer:               cfg.WS.EventBuffer,
			DropPolicy:                cfg.WS.DropPolicy,
			RawCaptureEnabled:         cfg.WS.RawCaptureEnabled,
			RawCaptureMaxBytes:        cfg.WS.RawCaptureMaxBytes,
			ReconcileEnabled:          cfg.WS.ReconcileEnabled,
			ReconcileInterval:         cfg.WS.ReconcileInterval,
			GapRecoveryLookback:       cfg.WS.GapRecoveryLookback,
			HealthStaleAfter:          cfg.WS.HealthStaleAfter,
			StartupSubscribeDelay:     cfg.WS.StartupSubscribeDelay,
			PriceMoveTrigger:          cfg.WS.PriceMoveTrigger,
			RepricingTriggerCooldown:  cfg.WS.RepricingTriggerCooldown,
			PredictionRefreshCooldown: cfg.WS.PredictionRefreshCooldown,
			Endpoint:                  cfg.WS.Endpoint,
		}, realtimeStore, wsClient, selector.Select, met, logger)
		logger.Info().
			Bool("enabled", cfg.WS.Enabled).
			Str("mode", cfg.WS.SubscriptionMode).
			Int("max_markets", cfg.WS.MaxMarkets).
			Str("endpoint", cfg.WS.Endpoint).
			Msg("realtime ws: wired")
	}

	// v10.0 Prediction Creation Worker (cold-start path). Without
	// this loop, the evolution worker has nothing to evolve.
	// v11.2 cleanup: prediction creation worker fully removed.
	// The output-product variant (Telegram + AI-driven thesis) is
	// gone; the only surviving prediction-related surface is the
	// flow-alert AI context (catalyst stamping), which does not
	// require this worker.

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
		detection:         detectionWorker,
		aiAnalysis:        aiSvc,
		outcomeAI:         outcomeAIWorker,
		catalystImporter:  catalystImporterWorker,
		newsIntel:         newsIntelWorker,
		signalQuality:     signalQualityWorker,
		marketCloseReview: marketCloseReviewWorker,
		realtimeWS:        realtimeWorker,
		strategyPhaseB:    strategyPhaseB,
		strategyPhaseC:    strategyPhaseC,
		strategyPhaseF:    strategyPhaseF,
		pgPool:            pgPool,
	}, nil
}

// v11.3: newsIntelTelegramAdapter removed — newsintel.Worker now
// takes the *telegram.Router directly via the typed Send seam.

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
	repo := repository.NewEventPageRepository(a_pgPool(marketsRepo))
	client, err := eventpage.NewClient(eventpage.ClientConfig{
		HTMLBaseURL:  cfg.EventPage.HTMLBaseURL,
		Resolver:     resolver,
		AliasStore:   repo, // v10.5: persistent canonical-slug aliases
		Logger:       logger,
		Metrics:      newEventPageMetricsSink(met),
		MaxRedirects: cfg.EventPage.MaxRedirects,
	})
	if err != nil {
		logger.Warn().Err(err).Msg("event page client: construction failed; narrative context disabled")
		return nil
	}
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
	if a.detection != nil {
		execs = append(execs, shutdown2.Exec{Name: "detection", Fn: func(ctx context.Context) error {
			a.detection.Run(ctx)
			return nil
		}})
	}
	if a.outcomeAI != nil {
		execs = append(execs, shutdown2.Exec{Name: "outcomeai", Fn: func(ctx context.Context) error {
			a.outcomeAI.Run(ctx)
			return nil
		}})
	}
	if a.catalystImporter != nil {
		execs = append(execs, shutdown2.Exec{Name: "catalyst-importer", Fn: func(ctx context.Context) error {
			a.catalystImporter.Run(ctx)
			return nil
		}})
	}
	if a.newsIntel != nil {
		execs = append(execs, shutdown2.Exec{Name: "news-intel", Fn: func(ctx context.Context) error {
			a.newsIntel.Run(ctx)
			return nil
		}})
	}
	if a.signalQuality != nil {
		execs = append(execs, shutdown2.Exec{Name: "signalquality-admin", Fn: a.signalQuality.Run})
	}
	if a.marketCloseReview != nil {
		execs = append(execs, shutdown2.Exec{Name: "market-close-review", Fn: func(ctx context.Context) error {
			a.marketCloseReview.Run(ctx)
			return nil
		}})
	}
	if a.realtimeWS != nil {
		execs = append(execs, shutdown2.Exec{Name: "realtime-ws", Fn: func(ctx context.Context) error {
			a.realtimeWS.Run(ctx)
			return nil
		}})
	}
	// v11.6 Phase B workers.
	if a.strategyPhaseB.ValueWorker != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-value-evaluator", Fn: func(ctx context.Context) error {
			a.strategyPhaseB.ValueWorker.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseB.PromotionRev != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-promotion-review", Fn: func(ctx context.Context) error {
			a.strategyPhaseB.PromotionRev.Run(ctx)
			return nil
		}})
	}
	// v11.7 Phase C workers — only registered when feature flags allow.
	if a.strategyPhaseC.MarketLinks != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-marketlinks", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.MarketLinks.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseC.HolderSync != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-holdersync", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.HolderSync.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseC.RiskScore != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-riskscore", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.RiskScore.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseC.Repricing != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-repricing", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.Repricing.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseC.WalletGraph != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-walletgraph", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.WalletGraph.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseC.OutcomeBackfill != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-outcome-backfill", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.OutcomeBackfill.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseF.BookBars != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-bookbars", Fn: func(ctx context.Context) error {
			a.strategyPhaseF.BookBars.Run(ctx)
			return nil
		}})
	}
	if a.strategyPhaseC.ThesisLines != nil {
		execs = append(execs, shutdown2.Exec{Name: "strategy-thesis-lines", Fn: func(ctx context.Context) error {
			a.strategyPhaseC.ThesisLines.Run(ctx)
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
