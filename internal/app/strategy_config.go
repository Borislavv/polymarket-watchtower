// strategy_config.go — v11.5 Strategy Learning Loop configuration.
//
// All nine detectors + five workers default to disabled / shadow-only.
// Nothing in this block can ever cause a Telegram alert by itself; the
// promotion path documented in CLAUDE.md is the only way a detector
// becomes live, and even then it goes through the existing alert
// pipeline (polymarket_alerts → alertsender.Worker → Telegram).
//
// Naming convention is per-strategy block + per-worker block. Every
// detector exposes <NAME>_ENABLED + <NAME>_SHADOW_ONLY so the
// operator can switch on shadow recording without flipping promotion.
package app

import "time"

// StrategyConfig nests every v11.5 detector + worker config under
// a single field on the root Config. Detectors are PURE — no
// reading happens here; the workers feed them.
type StrategyConfig struct {
	// StrategyVersion stamps every polymarket_strategy_shadow_decisions
	// row. Bump when the dedup namespace or the scorer shape changes.
	// Keep stable for tuning passes.
	StrategyVersion string `env:"STRATEGY_LEARNING_LOOP_VERSION" envDefault:"v11.5-shadow"`

	// PromotionReady is the kill-switch a future promotion job
	// flips per strategy to allow live emission. Defaults false in
	// every environment; promotion is operator-driven (see CLAUDE.md
	// "Strategy v11.5" section, promotion criteria).
	GlobalPromotionAllowed bool `env:"STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED" envDefault:"false"`

	// v11.6/v11.8 detect.Loop hook knobs (now env-bound).
	ShadowMaxDecisionsPerTrade int  `env:"STRATEGY_SHADOW_MAX_DECISIONS_PER_TRADE" envDefault:"20" validate:"gte=1,lte=100"`
	ShadowRecordNoFire         bool `env:"STRATEGY_SHADOW_RECORD_NOFIRE" envDefault:"false"`

	// --- v11.7 PART 9: promotion-review thresholds ---
	// All four are now env-driven. Defaults match the v11.6
	// hardcoded values so existing operators see no behaviour
	// change without explicit overrides.
	PromotionMinSample            int           `env:"STRATEGY_PROMOTION_MIN_SAMPLE" envDefault:"50" validate:"gte=1"`
	PromotionMinSignedMove6hCents float64       `env:"STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS" envDefault:"1.5" validate:"gte=0"`
	PromotionMaxReversal15mRatio  float64       `env:"STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO" envDefault:"0.5" validate:"gte=0,lte=1"`
	PromotionMaxAlertsPerDay      float64       `env:"STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY" envDefault:"40" validate:"gte=0"`
	PromotionBypassExplicit       bool          `env:"STRATEGY_PROMOTION_BYPASS_EXPLICIT" envDefault:"false"`
	PromotionReviewInterval       time.Duration `env:"STRATEGY_PROMOTION_REVIEW_INTERVAL" envDefault:"1h" validate:"gt=0"`
	PromotionReviewLookback       time.Duration `env:"STRATEGY_PROMOTION_REVIEW_LOOKBACK" envDefault:"336h" validate:"gt=0"`

	ThesisAccum     ThesisAccumConfig
	OwnershipV2     OwnershipV2Config
	CatalystWindow  CatalystWindowConfig
	BookVacuum      BookVacuumConfig
	RepricingLag    RepricingLagConfig
	WalletCohort    WalletCohortConfig
	ConflictResolve ConflictResolveConfig
	RulesRisk       RulesRiskConfig
	CheapTail       CheapTailConfig

	MarketLinks     MarketLinksConfig
	HolderSync      HolderSyncConfig
	RiskScore       RiskScoreConfig
	Repricing       RepricingWorkerConfig
	WalletGraph     WalletGraphConfig
	OutcomeBackfill OutcomeBackfillConfig
	StagedInputs    StagedInputsConfig
	// v11.9 additions
	BookFeatureBars BookFeatureBarsConfig
	ThesisLines     ThesisLinesConfig
	// v11.10 additions
	TelegramFlow StrategyTelegramFlowConfig
}

// OutcomeBackfillConfig — strategyoutcome worker.
//
// v11.9: ResolveSide flips on per-side correct/wrong resolution via
// the alert's winning_outcome_token + the shadow row's Side. When
// false, closed markets fall back to outcome_status='unknown'.
type OutcomeBackfillConfig struct {
	Enabled           bool          `env:"STRATEGY_OUTCOME_EVALUATOR_ENABLED" envDefault:"true"`
	Interval          time.Duration `env:"STRATEGY_OUTCOME_EVALUATOR_INTERVAL" envDefault:"1h" validate:"gt=0"`
	BatchSize         int           `env:"STRATEGY_OUTCOME_EVALUATOR_BATCH_SIZE" envDefault:"1000" validate:"gte=1,lte=10000"`
	StandaloneEnabled bool          `env:"STRATEGY_OUTCOME_STANDALONE_ENABLED" envDefault:"true"`
	StandaloneBatch   int           `env:"STRATEGY_OUTCOME_STANDALONE_BATCH_SIZE" envDefault:"1000" validate:"gte=1,lte=10000"`
	ResolveSide       bool          `env:"STRATEGY_OUTCOME_STANDALONE_RESOLVE_SIDE" envDefault:"true"`
}

// StrategyTelegramFlowConfig — v11.10 admin/user Telegram split.
//
// Strategy decisions go through ONE centralized sender; the flow
// (admin vs user) is decided here, not in detectors.
//
// Routing rules:
//   - shadow decisions (`shadow_only=true`) → admin flow only, when
//     `ShadowToAdmin=true`;
//   - promoted decisions (`shadow_only=false`) → user flow ONLY
//     when:
//   - UserFlowEnabled=true,
//   - confidence ≥ MinUserConfidence,
//   - decision_level ∈ {warning, critical, hard} (or LevelNone
//     downgraded by the router if MinUserLevel is set higher),
//   - STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true,
//   - promotion gate (StrategyPromotion review) eligible,
//   - additionally promoted decisions ALSO mirror to admin when
//     `PromotedToUser=true` (admin sees what user sees);
//   - any other surface (Watchtower stats, market_intel, daily
//     intel, prediction_update blocked, top annotations) is
//     DISABLED here regardless of upstream — see
//     `staleEnvKeys` reject + `*_TELEGRAM_ENABLED=false`.
type StrategyTelegramFlowConfig struct {
	AdminEnabled      bool          `env:"TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED" envDefault:"true"`
	UserEnabled       bool          `env:"TELEGRAM_STRATEGY_USER_FLOW_ENABLED" envDefault:"false"`
	ShadowToAdmin     bool          `env:"TELEGRAM_STRATEGY_SHADOW_TO_ADMIN" envDefault:"true"`
	PromotedToUser    bool          `env:"TELEGRAM_STRATEGY_PROMOTED_TO_USER" envDefault:"true"`
	MinUserConfidence float64       `env:"TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE" envDefault:"0.75" validate:"gte=0,lte=1"`
	MinUserLevel      string        `env:"TELEGRAM_STRATEGY_MIN_USER_LEVEL" envDefault:"warning"`
	UserDedupeWindow  time.Duration `env:"TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW" envDefault:"12h" validate:"gt=0"`
	AdminDedupeWindow time.Duration `env:"TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW" envDefault:"1h" validate:"gt=0"`
}

// StagedInputsConfig — v11.8 detect.Loop staged-input bridge.
type StagedInputsConfig struct {
	Enabled      bool          `env:"STRATEGY_STAGED_INPUTS_ENABLED" envDefault:"true"`
	CacheEnabled bool          `env:"STRATEGY_STAGED_CACHE_ENABLED" envDefault:"true"`
	CacheTTL     time.Duration `env:"STRATEGY_STAGED_CACHE_TTL" envDefault:"60s" validate:"gt=0"`
	MaxRows      int           `env:"STRATEGY_STAGED_MAX_QUERY_ROWS" envDefault:"200" validate:"gte=1,lte=10000"`
	QueryTimeout time.Duration `env:"STRATEGY_STAGED_QUERY_TIMEOUT" envDefault:"250ms" validate:"gt=0"`
}

// ThesisAccumConfig — Strategy 1 (primary).
// Cross-market thesis accumulation. Wallet conviction across linked
// markets inside a thesis graph.
type ThesisAccumConfig struct {
	Enabled           bool          `env:"THESIS_ACCUM_ENABLED" envDefault:"false"`
	ShadowOnly        bool          `env:"THESIS_ACCUM_SHADOW_ONLY" envDefault:"true"`
	LookbackRecent    time.Duration `env:"THESIS_ACCUM_LOOKBACK_RECENT" envDefault:"72h" validate:"gt=0"`
	LookbackLifetime  time.Duration `env:"THESIS_ACCUM_LOOKBACK_LIFETIME" envDefault:"8760h" validate:"gt=0"`
	MinBreadth        int           `env:"THESIS_ACCUM_MIN_BREADTH" envDefault:"2" validate:"gte=2,lte=50"`
	MinConsistency    float64       `env:"THESIS_ACCUM_MIN_CONSISTENCY" envDefault:"0.75" validate:"gte=0.5,lte=1"`
	MinAlignedScore   float64       `env:"THESIS_ACCUM_MIN_ALIGNED_SCORE" envDefault:"1.5" validate:"gte=0"`
	CatalystBoostMax  float64       `env:"THESIS_ACCUM_CATALYST_BOOST_MAX" envDefault:"0.4" validate:"gte=0,lte=2"`
	LiquidityFloorUSD float64       `env:"THESIS_ACCUM_LIQUIDITY_FLOOR_USD" envDefault:"500" validate:"gte=0"`
	MaxLinkedMarkets  int           `env:"THESIS_ACCUM_MAX_LINKED_MARKETS" envDefault:"32" validate:"gte=2,lte=200"`
}

// OwnershipV2Config — Strategy 2 (primary).
// True holder-delta concentration. Replaces v6 approximate ownership.
type OwnershipV2Config struct {
	Enabled              bool          `env:"OWNERSHIP_V2_ENABLED" envDefault:"false"`
	ShadowOnly           bool          `env:"OWNERSHIP_V2_SHADOW_ONLY" envDefault:"true"`
	MinPctOIInfo         float64       `env:"OWNERSHIP_V2_MIN_PCT_OI_INFO" envDefault:"0.03" validate:"gt=0,lt=1"`
	MinPctOIWarn         float64       `env:"OWNERSHIP_V2_MIN_PCT_OI_WARN" envDefault:"0.08" validate:"gt=0,lt=1"`
	MinPctOICrit         float64       `env:"OWNERSHIP_V2_MIN_PCT_OI_CRIT" envDefault:"0.15" validate:"gt=0,lt=1"`
	TopK                 int           `env:"OWNERSHIP_V2_TOPK" envDefault:"5" validate:"gte=1,lte=50"`
	MinSharesDelta       float64       `env:"OWNERSHIP_V2_MIN_SHARES_DELTA" envDefault:"500" validate:"gte=0"`
	FreshSnapshotMaxAge  time.Duration `env:"OWNERSHIP_V2_FRESH_SNAPSHOT_MAX_AGE" envDefault:"2h" validate:"gt=0"`
	DenominatorPenaltyOI float64       `env:"OWNERSHIP_V2_DENOMINATOR_PENALTY_OI" envDefault:"0.3" validate:"gte=0,lte=1"`
	V1ApproxEnabled      bool          `env:"OWNERSHIP_V1_APPROX_ENABLED" envDefault:"true"`
}

// CatalystWindowConfig — Strategy 5 (booster).
// Never standalone. Boosts when signal lands inside a configured
// catalyst proximity window.
type CatalystWindowConfig struct {
	Enabled       bool    `env:"CATALYST_WINDOW_ENABLED" envDefault:"false"`
	ShadowOnly    bool    `env:"CATALYST_WINDOW_SHADOW_ONLY" envDefault:"true"`
	MinConfidence float64 `env:"CATALYST_WINDOW_MIN_CONFIDENCE" envDefault:"0.5" validate:"gte=0,lte=1"`

	DebatePre             time.Duration `env:"CATALYST_WINDOW_DEBATE_PRE" envDefault:"12h" validate:"gt=0"`
	DebatePost            time.Duration `env:"CATALYST_WINDOW_DEBATE_POST" envDefault:"4h" validate:"gt=0"`
	CourtRulingPre        time.Duration `env:"CATALYST_WINDOW_COURT_RULING_PRE" envDefault:"24h" validate:"gt=0"`
	CourtRulingPost       time.Duration `env:"CATALYST_WINDOW_COURT_RULING_POST" envDefault:"12h" validate:"gt=0"`
	ElectionDayPre        time.Duration `env:"CATALYST_WINDOW_ELECTION_DAY_PRE" envDefault:"72h" validate:"gt=0"`
	ElectionDayPost       time.Duration `env:"CATALYST_WINDOW_ELECTION_DAY_POST" envDefault:"24h" validate:"gt=0"`
	OfficialStatementPre  time.Duration `env:"CATALYST_WINDOW_OFFICIAL_STATEMENT_PRE" envDefault:"4h" validate:"gt=0"`
	OfficialStatementPost time.Duration `env:"CATALYST_WINDOW_OFFICIAL_STATEMENT_POST" envDefault:"2h" validate:"gt=0"`
	GenericPre            time.Duration `env:"CATALYST_WINDOW_GENERIC_PRE" envDefault:"6h" validate:"gt=0"`
	GenericPost           time.Duration `env:"CATALYST_WINDOW_GENERIC_POST" envDefault:"3h" validate:"gt=0"`
}

// BookVacuumConfig — Strategy 6 (booster).
type BookVacuumConfig struct {
	Enabled        bool          `env:"BOOK_VACUUM_ENABLED" envDefault:"false"`
	ShadowOnly     bool          `env:"BOOK_VACUUM_SHADOW_ONLY" envDefault:"true"`
	TopN           int           `env:"BOOK_VACUUM_TOPN" envDefault:"5" validate:"gte=1,lte=50"`
	MinCollapsePct float64       `env:"BOOK_VACUUM_MIN_COLLAPSE_PCT" envDefault:"0.5" validate:"gt=0,lt=1"`
	MaxRestore     time.Duration `env:"BOOK_VACUUM_MAX_RESTORE_SEC" envDefault:"30s" validate:"gt=0"`
	MinSpreadZ     float64       `env:"BOOK_VACUUM_MIN_SPREAD_Z" envDefault:"1.5" validate:"gt=0"`
	MinMidShiftPct float64       `env:"BOOK_VACUUM_MIN_MID_SHIFT_PCT" envDefault:"0.01" validate:"gte=0"`
	MaxAgeBar      time.Duration `env:"BOOK_VACUUM_MAX_AGE_BAR" envDefault:"5m" validate:"gt=0"`
}

// RepricingLagConfig — Strategy 3 (primary).
type RepricingLagConfig struct {
	Enabled         bool          `env:"REPRICING_LAG_ENABLED" envDefault:"false"`
	ShadowOnly      bool          `env:"REPRICING_LAG_SHADOW_ONLY" envDefault:"true"`
	MinLagCents     float64       `env:"REPRICING_LAG_MIN_CENTS" envDefault:"3" validate:"gt=0"`
	PeerMinCount    int           `env:"REPRICING_LAG_PEER_MIN_COUNT" envDefault:"2" validate:"gte=1,lte=50"`
	CheckWindowsCSV string        `env:"REPRICING_LAG_CHECK_WINDOWS" envDefault:"5m,15m,1h"`
	MaxAmbiguity    float64       `env:"REPRICING_LAG_MAX_AMBIGUITY" envDefault:"0.6" validate:"gte=0,lte=1"`
	OpenInterval    time.Duration `env:"REPRICING_LAG_OPEN_INTERVAL" envDefault:"30s" validate:"gt=0"`
	CloseGrace      time.Duration `env:"REPRICING_LAG_CLOSE_GRACE" envDefault:"2m" validate:"gt=0"`
}

// WalletCohortConfig — Strategy 7 (booster).
type WalletCohortConfig struct {
	Enabled           bool          `env:"WALLET_COHORT_ENABLED" envDefault:"false"`
	ShadowOnly        bool          `env:"WALLET_COHORT_SHADOW_ONLY" envDefault:"true"`
	MinSimilarity     float64       `env:"WALLET_COHORT_MIN_SIMILARITY" envDefault:"0.5" validate:"gt=0,lte=1"`
	MinEvents         int           `env:"WALLET_COHORT_MIN_EVENTS" envDefault:"3" validate:"gte=2,lte=50"`
	CoTradeWindow     time.Duration `env:"WALLET_COHORT_COTRADE_WINDOW" envDefault:"30m" validate:"gt=0"`
	UseFundingEdges   bool          `env:"WALLET_COHORT_USE_FUNDING_EDGES" envDefault:"false"`
	ConvergenceWindow time.Duration `env:"WALLET_COHORT_CONVERGENCE_WINDOW" envDefault:"4h" validate:"gt=0"`
	MinCohortHits     int           `env:"WALLET_COHORT_MIN_COHORT_HITS" envDefault:"2" validate:"gte=2,lte=50"`
}

// ConflictResolveConfig — Strategy 8 (arbitration).
type ConflictResolveConfig struct {
	Enabled       bool          `env:"CONFLICT_RESOLVE_ENABLED" envDefault:"false"`
	ShadowOnly    bool          `env:"CONFLICT_RESOLVE_SHADOW_ONLY" envDefault:"true"`
	Window        time.Duration `env:"CONFLICT_RESOLVE_WINDOW" envDefault:"15m" validate:"gt=0"`
	MinDominance  float64       `env:"CONFLICT_RESOLVE_MIN_DOMINANCE" envDefault:"1.5" validate:"gt=1"`
	MMPenalty     float64       `env:"CONFLICT_RESOLVE_MM_PENALTY" envDefault:"0.4" validate:"gte=0,lte=2"`
	MinQualitySum float64       `env:"CONFLICT_RESOLVE_MIN_QUALITY_SUM" envDefault:"1.0" validate:"gte=0"`
}

// RulesRiskConfig — Strategy 9 (safety/suppressor).
type RulesRiskConfig struct {
	Enabled            bool    `env:"RULES_RISK_ENABLED" envDefault:"false"`
	ShadowOnly         bool    `env:"RULES_RISK_SHADOW_ONLY" envDefault:"true"`
	HighThreshold      float64 `env:"RULES_RISK_HIGH_THRESHOLD" envDefault:"0.6" validate:"gt=0,lte=1"`
	MidThreshold       float64 `env:"RULES_RISK_MID_THRESHOLD" envDefault:"0.3" validate:"gt=0,lte=1"`
	HighCapSeverity    string  `env:"RULES_RISK_HIGH_CAP_SEVERITY" envDefault:"warning"`
	BlockRepricingHigh bool    `env:"RULES_RISK_BLOCK_REPRICING" envDefault:"true"`
	BlockCheapTailHigh bool    `env:"RULES_RISK_BLOCK_CHEAPTAIL" envDefault:"true"`
}

// CheapTailConfig — Strategy 4 (primary).
type CheapTailConfig struct {
	Enabled         bool    `env:"CHEAPTAIL_ENABLED" envDefault:"false"`
	ShadowOnly      bool    `env:"CHEAPTAIL_SHADOW_ONLY" envDefault:"true"`
	MinProb         float64 `env:"CHEAPTAIL_MIN_PROB" envDefault:"0.02" validate:"gt=0,lt=1"`
	MaxProb         float64 `env:"CHEAPTAIL_MAX_PROB" envDefault:"0.15" validate:"gt=0,lt=1"`
	MinNotionalUSD  float64 `env:"CHEAPTAIL_MIN_NOTIONAL_USD" envDefault:"1000" validate:"gte=0"`
	MinTrades       int     `env:"CHEAPTAIL_MIN_TRADES" envDefault:"3" validate:"gte=1,lte=200"`
	RequireCatalyst bool    `env:"CHEAPTAIL_REQUIRE_CATALYST" envDefault:"true"`
	AmbiguityCutoff float64 `env:"CHEAPTAIL_AMBIGUITY_CUTOFF" envDefault:"0.7" validate:"gte=0,lte=1"`
}

// MarketLinksConfig — marketlinks.Builder worker.
type MarketLinksConfig struct {
	Enabled        bool          `env:"MARKETLINKS_ENABLED" envDefault:"false"`
	Interval       time.Duration `env:"MARKETLINKS_INTERVAL" envDefault:"30m" validate:"gt=0"`
	BatchSize      int           `env:"MARKETLINKS_BATCH_SIZE" envDefault:"100" validate:"gte=1,lte=2000"`
	LinkVersion    int           `env:"MARKETLINKS_LINK_VERSION" envDefault:"1" validate:"gte=1"`
	IncludeOpposed bool          `env:"MARKETLINKS_INCLUDE_OPPOSED" envDefault:"true"`
	MinConfidence  float64       `env:"MARKETLINKS_MIN_CONFIDENCE" envDefault:"0.3" validate:"gte=0,lte=1"`
}

// HolderSyncConfig — holdersync.Worker.
//
// v11.9 added SourceMode + RequireOpenInterest + RateLimitRPS knobs.
// SourceMode=disabled (default) keeps the worker inert; SourceMode=
// dataapi will activate the adapter ONCE a verified Polymarket
// holders endpoint is wrapped. SourceMode=auto picks dataapi when
// the client exists, otherwise stays disabled with a clear metric.
type HolderSyncConfig struct {
	Enabled             bool          `env:"OWNERSHIP_SYNC_ENABLED" envDefault:"false"`
	WorkerEnabled       bool          `env:"HOLDERSYNC_WORKER_ENABLED" envDefault:"false"`
	SourceMode          string        `env:"HOLDERSYNC_SOURCE_MODE" envDefault:"disabled"`
	Interval            time.Duration `env:"OWNERSHIP_SYNC_INTERVAL" envDefault:"20m" validate:"gt=0"`
	IntervalV2          time.Duration `env:"HOLDERSYNC_INTERVAL" envDefault:"10m" validate:"gt=0"`
	MaxMarkets          int           `env:"OWNERSHIP_SYNC_MAX_MARKETS" envDefault:"50" validate:"gte=1,lte=2000"`
	MaxMarketsV2        int           `env:"HOLDERSYNC_MAX_MARKETS" envDefault:"250" validate:"gte=1,lte=2000"`
	TopK                int           `env:"OWNERSHIP_SYNC_TOPK" envDefault:"25" validate:"gte=1,lte=200"`
	TopKV2              int           `env:"HOLDERSYNC_TOPK" envDefault:"25" validate:"gte=1,lte=200"`
	FetchTimeout        time.Duration `env:"OWNERSHIP_SYNC_TIMEOUT" envDefault:"15s" validate:"gt=0"`
	PerMarketTimeout    time.Duration `env:"HOLDERSYNC_PER_MARKET_TIMEOUT" envDefault:"5s" validate:"gt=0"`
	Concurrency         int           `env:"OWNERSHIP_SYNC_CONCURRENCY" envDefault:"3" validate:"gte=1,lte=32"`
	StaleAfter          time.Duration `env:"OWNERSHIP_SYNC_STALE_AFTER" envDefault:"6h" validate:"gt=0"`
	RateLimitRPS        float64       `env:"HOLDERSYNC_RATE_LIMIT_RPS" envDefault:"2" validate:"gte=0,lte=100"`
	RequireOpenInterest bool          `env:"HOLDERSYNC_REQUIRE_OPEN_INTEREST" envDefault:"true"`
}

// BookFeatureBarsConfig — v11.9 producer for polymarket_book_feature_bars.
// Producer itself is Phase E (requires WS Event depth field
// extension + aggregator). Knobs exist so the operator surface is
// stable when the producer lands.
type BookFeatureBarsConfig struct {
	Enabled               bool          `env:"BOOK_FEATURE_BARS_ENABLED" envDefault:"false"`
	Interval              time.Duration `env:"BOOK_FEATURE_BARS_INTERVAL" envDefault:"5s" validate:"gt=0"`
	TopN                  int           `env:"BOOK_FEATURE_BARS_TOPN" envDefault:"5" validate:"gte=1,lte=50"`
	MaxMarkets            int           `env:"BOOK_FEATURE_BARS_MAX_MARKETS" envDefault:"250" validate:"gte=1,lte=5000"`
	RequireDepthForVacuum bool          `env:"BOOK_FEATURE_BARS_REQUIRE_DEPTH_FOR_VACUUM" envDefault:"true"`
	Retention             time.Duration `env:"BOOK_FEATURE_BARS_RETENTION" envDefault:"720h" validate:"gt=0"`
}

// ThesisLinesConfig — v11.9 background matrix that precomputes
// wallet directional exposure across linked markets. Hot-path
// reader is bounded by HOTPATH_MAX_LINKED_MARKETS + a per-query
// timeout.
type ThesisLinesConfig struct {
	WorkerEnabled       bool          `env:"THESIS_LINES_WORKER_ENABLED" envDefault:"false"`
	Lookback            time.Duration `env:"THESIS_LINES_LOOKBACK" envDefault:"720h" validate:"gt=0"`
	Interval            time.Duration `env:"THESIS_LINES_INTERVAL" envDefault:"10m" validate:"gt=0"`
	MaxEvents           int           `env:"THESIS_LINES_MAX_EVENTS" envDefault:"500" validate:"gte=1,lte=10000"`
	MaxWallets          int           `env:"THESIS_LINES_MAX_WALLETS" envDefault:"10000" validate:"gte=1,lte=1000000"`
	HotpathMaxLinked    int           `env:"THESIS_HOTPATH_MAX_LINKED_MARKETS" envDefault:"25" validate:"gte=2,lte=200"`
	HotpathQueryTimeout time.Duration `env:"THESIS_HOTPATH_QUERY_TIMEOUT" envDefault:"250ms" validate:"gt=0"`
}

// RiskScoreConfig — riskscore.Worker.
type RiskScoreConfig struct {
	Enabled      bool          `env:"RISKSCORE_ENABLED" envDefault:"false"`
	Interval     time.Duration `env:"RISKSCORE_INTERVAL" envDefault:"15m" validate:"gt=0"`
	BatchSize    int           `env:"RISKSCORE_BATCH_SIZE" envDefault:"100" validate:"gte=1,lte=5000"`
	ScoreVersion int           `env:"RISKSCORE_VERSION" envDefault:"1" validate:"gte=1"`
	RefreshOlder time.Duration `env:"RISKSCORE_REFRESH_OLDER_THAN" envDefault:"24h" validate:"gt=0"`
}

// RepricingWorkerConfig — repricing.Worker.
//
// v11.9 added the close-phase knobs: CloseEnabled flips on the
// real target+peer sampler, PriceSource picks between snapshots/
// trades/auto, MinPeerCount + MinLagCents drive the lag classifier.
type RepricingWorkerConfig struct {
	Enabled        bool          `env:"REPRICING_WORKER_ENABLED" envDefault:"false"`
	Interval       time.Duration `env:"REPRICING_WORKER_INTERVAL" envDefault:"60s" validate:"gt=0"`
	OpenLookback   time.Duration `env:"REPRICING_WORKER_OPEN_LOOKBACK" envDefault:"15m" validate:"gt=0"`
	MaxOpenWindows int           `env:"REPRICING_WORKER_MAX_OPEN_WINDOWS" envDefault:"500" validate:"gte=1,lte=5000"`
	CloseAfter     time.Duration `env:"REPRICING_WORKER_CLOSE_AFTER" envDefault:"2h" validate:"gt=0"`
	CloseEnabled   bool          `env:"REPRICING_CLOSE_ENABLED" envDefault:"true"`
	MinPeerCount   int           `env:"REPRICING_MIN_PEER_COUNT" envDefault:"2" validate:"gte=1,lte=50"`
	MinLagCents    float64       `env:"REPRICING_MIN_LAG_CENTS" envDefault:"3" validate:"gt=0"`
	PriceSource    string        `env:"REPRICING_PRICE_SOURCE" envDefault:"trades"`
}

// WalletGraphConfig — walletgraph.Worker.
type WalletGraphConfig struct {
	Enabled            bool          `env:"WALLETGRAPH_ENABLED" envDefault:"false"`
	Interval           time.Duration `env:"WALLETGRAPH_INTERVAL" envDefault:"1h" validate:"gt=0"`
	CoTradeWindow      time.Duration `env:"WALLETGRAPH_COTRADE_WINDOW" envDefault:"30m" validate:"gt=0"`
	MinSharedEvents    int           `env:"WALLETGRAPH_MIN_SHARED_EVENTS" envDefault:"3" validate:"gte=2,lte=50"`
	BatchSize          int           `env:"WALLETGRAPH_BATCH_SIZE" envDefault:"5000" validate:"gte=100,lte=100000"`
	EdgeVersion        int           `env:"WALLETGRAPH_EDGE_VERSION" envDefault:"1" validate:"gte=1"`
	UseFundingProvider bool          `env:"WALLETGRAPH_USE_FUNDING_PROVIDER" envDefault:"false"`
}
