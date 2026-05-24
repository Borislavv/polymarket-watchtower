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

import (
	"fmt"
	"strings"
	"time"
)

// ParseRepricingLagWindows splits REPRICING_LAG_CHECK_WINDOWS
// ("5m,30m,1h,6h,24h") into a sorted ascending []time.Duration. An
// empty CSV returns an empty slice; an unparseable token returns an
// error so misconfiguration fails at boot, not at detect-time.
//
// v11.10-insider-prior requires 5m,30m,1h,6h,24h coverage; the
// 24h horizon implies REPRICING_WORKER_CLOSE_AFTER ≥ 24h (in fact
// the default is 26h — 2h grace). The
// TestRepricingLagWindows_FullHorizonCoverage test pins both
// invariants.
func (c RepricingLagConfig) ParsedCheckWindows() ([]time.Duration, error) {
	raw := strings.TrimSpace(c.CheckWindowsCSV)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]time.Duration, 0, len(parts))
	seen := map[time.Duration]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := time.ParseDuration(p)
		if err != nil {
			return nil, fmt.Errorf("REPRICING_LAG_CHECK_WINDOWS: parse %q: %w", p, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("REPRICING_LAG_CHECK_WINDOWS: window %q must be > 0", p)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	// Ascending sort: callers iterate earliest-first.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out, nil
}

// StrategyConfig nests every v11.5 detector + worker config under
// a single field on the root Config. Detectors are PURE — no
// reading happens here; the workers feed them.
type StrategyConfig struct {
	// StrategyVersion stamps every polymarket_strategy_shadow_decisions
	// row. Bump when the dedup namespace or the scorer shape changes.
	// Keep stable for tuning passes.
	StrategyVersion string `env:"STRATEGY_LEARNING_LOOP_VERSION" envDefault:"v11.10-insider-prior"`

	// GlobalPromotionAllowed is the kill-switch a future promotion job
	// flips per strategy to allow live emission. Defaults false in
	// every environment; promotion is operator-driven (see CLAUDE.md
	// "Strategy v11.5" section, promotion criteria).
	GlobalPromotionAllowed bool `env:"STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED" envDefault:"false"`

	// v11.6/v11.8 detect.Loop hook knobs (now env-bound).
	ShadowMaxDecisionsPerTrade int  `env:"STRATEGY_SHADOW_MAX_DECISIONS_PER_TRADE" envDefault:"12" validate:"gte=1,lte=100"`
	ShadowRecordNoFire         bool `env:"STRATEGY_SHADOW_RECORD_NOFIRE" envDefault:"false"`

	// --- v11.7 PART 9: promotion-review thresholds ---
	// v11.10-insider-prior raises the floor (was median 1.5c / reversal
	// 0.5 / alerts-per-day 40). The new target is "real edge, real
	// quiet": median signed_move_6h ≥ 2.0c, reversal ratio ≤ 0.35,
	// alerts/day ≤ 15. Tests in strategy_config_test.go pin these.
	PromotionMinSample            int     `env:"STRATEGY_PROMOTION_MIN_SAMPLE" envDefault:"50" validate:"gte=1"`
	PromotionMinSignedMove6hCents float64 `env:"STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS" envDefault:"2.0" validate:"gte=0"`
	PromotionMaxReversal15mRatio  float64 `env:"STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO" envDefault:"0.35" validate:"gte=0,lte=1"`
	PromotionMaxAlertsPerDay      float64 `env:"STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY" envDefault:"15" validate:"gte=0"`
	// PromotionBypassExplicit — HISTORICAL NAME, FORCE-DISABLE SEMANTICS.
	//
	// Despite the name "bypass", this knob does NOT enable a bypass of
	// the promotion gate. When set to true, the PromotionGate.Allow()
	// implementation returns false for EVERY strategy regardless of
	// review state — i.e. it is a kill-switch that explicitly forbids
	// any live emission. The canonical alias
	// `STRATEGY_PROMOTION_FORCE_DISABLE` is read first; if absent, the
	// legacy `STRATEGY_PROMOTION_BYPASS_EXPLICIT` is honored. Either
	// being true forces the gate closed (logical OR).
	//
	// See: TestPromotionBypassExplicit_IsForceDisable in
	// strategypromotion_gate_test.go.
	PromotionForceDisable   bool          `env:"STRATEGY_PROMOTION_FORCE_DISABLE" envDefault:"false"`
	PromotionBypassExplicit bool          `env:"STRATEGY_PROMOTION_BYPASS_EXPLICIT" envDefault:"false"`
	PromotionReviewInterval time.Duration `env:"STRATEGY_PROMOTION_REVIEW_INTERVAL" envDefault:"1h" validate:"gt=0"`
	PromotionReviewLookback time.Duration `env:"STRATEGY_PROMOTION_REVIEW_LOOKBACK" envDefault:"336h" validate:"gt=0"`

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
	// v11.10 PART 6 — worker priority-bucket budgeting.
	WorkerBudget WorkerBudgetConfig
}

// WorkerBudgetConfig — v11.10 PART 6 priority-bucket caps used by
// strategy-supporting workers (holdersync, bookbars). The selector
// (internal/app/usecase/workerbudget) issues one bucketed Postgres
// query per cycle. A zero cap disables that bucket entirely; when
// every cap is zero AND no pins are supplied, workers fall back to
// the legacy unbucketed lister so the surface is backward-compatible.
//
// Bucket priorities (1=highest):
//
//	1 = operator-pinned (explicit condition ids)
//	2 = recent-alert (any market fired in last 24h)
//	3 = catalyst-near (active/expected catalyst, ≤72h out)
//	4 = linked-to-fired (market_links neighbour of fired alert)
//	5 = liquid (top by Polymarket liquidity, 24h event-page snapshot)
//	6 = fallback active (last_seen_at DESC, safety net)
type WorkerBudgetConfig struct {
	OperatorPinned     int      `env:"WORKER_BUDGET_OPERATOR_PINNED_MARKETS" envDefault:"20" validate:"gte=0,lte=2000"`
	RecentAlert        int      `env:"WORKER_BUDGET_RECENT_ALERT_MARKETS" envDefault:"30" validate:"gte=0,lte=2000"`
	CatalystNear       int      `env:"WORKER_BUDGET_CATALYST_NEAR_MARKETS" envDefault:"40" validate:"gte=0,lte=2000"`
	LinkedToFired      int      `env:"WORKER_BUDGET_LINKED_TO_FIRED_MARKETS" envDefault:"30" validate:"gte=0,lte=2000"`
	Liquid             int      `env:"WORKER_BUDGET_LIQUID_MARKETS" envDefault:"50" validate:"gte=0,lte=2000"`
	FallbackActive     int      `env:"WORKER_BUDGET_FALLBACK_ACTIVE_MARKETS" envDefault:"20" validate:"gte=0,lte=2000"`
	PinnedConditionIDs []string `env:"WORKER_OPERATOR_PINNED_CONDITION_IDS" envSeparator:"," envDefault:""`
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
	MinUserConfidence float64       `env:"TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE" envDefault:"0.80" validate:"gte=0,lte=1"`
	MinUserLevel      string        `env:"TELEGRAM_STRATEGY_MIN_USER_LEVEL" envDefault:"warning"`
	UserDedupeWindow  time.Duration `env:"TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW" envDefault:"12h" validate:"gt=0"`
	AdminDedupeWindow time.Duration `env:"TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW" envDefault:"1h" validate:"gt=0"`
}

// StagedInputsConfig — v11.8 detect.Loop staged-input bridge.
type StagedInputsConfig struct {
	Enabled      bool          `env:"STRATEGY_STAGED_INPUTS_ENABLED" envDefault:"true"`
	CacheEnabled bool          `env:"STRATEGY_STAGED_CACHE_ENABLED" envDefault:"true"`
	CacheTTL     time.Duration `env:"STRATEGY_STAGED_CACHE_TTL" envDefault:"45s" validate:"gt=0"`
	MaxRows      int           `env:"STRATEGY_STAGED_MAX_QUERY_ROWS" envDefault:"250" validate:"gte=1,lte=10000"`
	QueryTimeout time.Duration `env:"STRATEGY_STAGED_QUERY_TIMEOUT" envDefault:"250ms" validate:"gt=0"`
}

// ThesisAccumConfig — Strategy 1 (primary).
// Cross-market thesis accumulation. Wallet conviction across linked
// markets inside a thesis graph.
//
// v11.10-insider-prior defaults raise the consistency floor (0.75 →
// 0.82), tighten lifetime lookback to 90d to keep the line current,
// and increase the liquidity floor (500 → 1000) so the
// ln(1+aligned/floor) score isn't dominated by micro-volume.
// LookbackLifetime is locked to 2160h to match THESIS_LINES_LOOKBACK
// (the background-matrix worker) — divergence between the two is a
// hot-path correctness bug.
type ThesisAccumConfig struct {
	Enabled           bool          `env:"THESIS_ACCUM_ENABLED" envDefault:"true"`
	ShadowOnly        bool          `env:"THESIS_ACCUM_SHADOW_ONLY" envDefault:"true"`
	LookbackRecent    time.Duration `env:"THESIS_ACCUM_LOOKBACK_RECENT" envDefault:"48h" validate:"gt=0"`
	LookbackLifetime  time.Duration `env:"THESIS_ACCUM_LOOKBACK_LIFETIME" envDefault:"2160h" validate:"gt=0"`
	MinBreadth        int           `env:"THESIS_ACCUM_MIN_BREADTH" envDefault:"2" validate:"gte=2,lte=50"`
	MinConsistency    float64       `env:"THESIS_ACCUM_MIN_CONSISTENCY" envDefault:"0.82" validate:"gte=0.5,lte=1"`
	MinAlignedScore   float64       `env:"THESIS_ACCUM_MIN_ALIGNED_SCORE" envDefault:"2.0" validate:"gte=0"`
	CatalystBoostMax  float64       `env:"THESIS_ACCUM_CATALYST_BOOST_MAX" envDefault:"0.55" validate:"gte=0,lte=2"`
	LiquidityFloorUSD float64       `env:"THESIS_ACCUM_LIQUIDITY_FLOOR_USD" envDefault:"1000" validate:"gte=0"`
	MaxLinkedMarkets  int           `env:"THESIS_ACCUM_MAX_LINKED_MARKETS" envDefault:"24" validate:"gte=2,lte=200"`
}

// OwnershipV2Config — Strategy 2 (primary).
// True holder-delta concentration. Replaces v6 approximate ownership.
//
// v11.10-insider-prior loosens the info floor (0.03→0.02) to surface
// earlier holder positioning while tightening warn/crit so high tiers
// remain real conviction. TopK raised to 10 (still ≤ the upstream
// Polymarket /holders cap of 20 — see HolderSyncConfig.TopKV2).
type OwnershipV2Config struct {
	Enabled              bool          `env:"OWNERSHIP_V2_ENABLED" envDefault:"true"`
	ShadowOnly           bool          `env:"OWNERSHIP_V2_SHADOW_ONLY" envDefault:"true"`
	MinPctOIInfo         float64       `env:"OWNERSHIP_V2_MIN_PCT_OI_INFO" envDefault:"0.02" validate:"gt=0,lt=1"`
	MinPctOIWarn         float64       `env:"OWNERSHIP_V2_MIN_PCT_OI_WARN" envDefault:"0.06" validate:"gt=0,lt=1"`
	MinPctOICrit         float64       `env:"OWNERSHIP_V2_MIN_PCT_OI_CRIT" envDefault:"0.12" validate:"gt=0,lt=1"`
	TopK                 int           `env:"OWNERSHIP_V2_TOPK" envDefault:"10" validate:"gte=1,lte=20"`
	MinSharesDelta       float64       `env:"OWNERSHIP_V2_MIN_SHARES_DELTA" envDefault:"250" validate:"gte=0"`
	FreshSnapshotMaxAge  time.Duration `env:"OWNERSHIP_V2_FRESH_SNAPSHOT_MAX_AGE" envDefault:"45m" validate:"gt=0"`
	DenominatorPenaltyOI float64       `env:"OWNERSHIP_V2_DENOMINATOR_PENALTY_OI" envDefault:"0.40" validate:"gte=0,lte=1"`
	V1ApproxEnabled      bool          `env:"OWNERSHIP_V1_APPROX_ENABLED" envDefault:"true"`
}

// CatalystWindowConfig — Strategy 5 (booster).
// Never standalone. Boosts when signal lands inside a configured
// catalyst proximity window.
//
// v11.10-insider-prior raises the confidence floor (0.5→0.65) so
// only operator-confirmed catalysts produce a boost; per-kind windows
// tightened to insider-prior shapes (e.g. debate 8h/3h, court ruling
// 18h/8h, election 48h/12h, official statement 6h/2h, generic 3h/2h).
type CatalystWindowConfig struct {
	Enabled       bool    `env:"CATALYST_WINDOW_ENABLED" envDefault:"true"`
	ShadowOnly    bool    `env:"CATALYST_WINDOW_SHADOW_ONLY" envDefault:"true"`
	MinConfidence float64 `env:"CATALYST_WINDOW_MIN_CONFIDENCE" envDefault:"0.65" validate:"gte=0,lte=1"`

	DebatePre             time.Duration `env:"CATALYST_WINDOW_DEBATE_PRE" envDefault:"8h" validate:"gt=0"`
	DebatePost            time.Duration `env:"CATALYST_WINDOW_DEBATE_POST" envDefault:"3h" validate:"gt=0"`
	CourtRulingPre        time.Duration `env:"CATALYST_WINDOW_COURT_RULING_PRE" envDefault:"18h" validate:"gt=0"`
	CourtRulingPost       time.Duration `env:"CATALYST_WINDOW_COURT_RULING_POST" envDefault:"8h" validate:"gt=0"`
	ElectionDayPre        time.Duration `env:"CATALYST_WINDOW_ELECTION_DAY_PRE" envDefault:"48h" validate:"gt=0"`
	ElectionDayPost       time.Duration `env:"CATALYST_WINDOW_ELECTION_DAY_POST" envDefault:"12h" validate:"gt=0"`
	OfficialStatementPre  time.Duration `env:"CATALYST_WINDOW_OFFICIAL_STATEMENT_PRE" envDefault:"6h" validate:"gt=0"`
	OfficialStatementPost time.Duration `env:"CATALYST_WINDOW_OFFICIAL_STATEMENT_POST" envDefault:"2h" validate:"gt=0"`
	GenericPre            time.Duration `env:"CATALYST_WINDOW_GENERIC_PRE" envDefault:"3h" validate:"gt=0"`
	GenericPost           time.Duration `env:"CATALYST_WINDOW_GENERIC_POST" envDefault:"2h" validate:"gt=0"`
}

// BookVacuumConfig — Strategy 6 (booster).
//
// v11.10-insider-prior raises collapse / spread / mid thresholds so
// only meaningful depth withdrawals score; MaxRestore tightened to
// 20s and MaxAgeBar lowered to 90s so stale bars are rejected.
type BookVacuumConfig struct {
	Enabled        bool          `env:"BOOK_VACUUM_ENABLED" envDefault:"true"`
	ShadowOnly     bool          `env:"BOOK_VACUUM_SHADOW_ONLY" envDefault:"true"`
	TopN           int           `env:"BOOK_VACUUM_TOPN" envDefault:"5" validate:"gte=1,lte=50"`
	MinCollapsePct float64       `env:"BOOK_VACUUM_MIN_COLLAPSE_PCT" envDefault:"0.65" validate:"gt=0,lt=1"`
	MaxRestore     time.Duration `env:"BOOK_VACUUM_MAX_RESTORE_SEC" envDefault:"20s" validate:"gt=0"`
	MinSpreadZ     float64       `env:"BOOK_VACUUM_MIN_SPREAD_Z" envDefault:"2.0" validate:"gt=0"`
	MinMidShiftPct float64       `env:"BOOK_VACUUM_MIN_MID_SHIFT_PCT" envDefault:"0.015" validate:"gte=0"`
	MaxAgeBar      time.Duration `env:"BOOK_VACUUM_MAX_AGE_BAR" envDefault:"90s" validate:"gt=0"`
}

// RepricingLagConfig — Strategy 3 (primary).
//
// v11.10-insider-prior extends horizon coverage from 5m,15m,1h to the
// full 5m,30m,1h,6h,24h ladder. The 24h horizon needs
// REPRICING_WORKER_CLOSE_AFTER ≥ 26h (verified in
// RepricingWorkerConfig). PeerMinCount raised to 3 (real peer
// agreement, not a 2-row coincidence). MaxAmbiguity tightened to
// 0.45 so rulesrisk-heavy markets stay blocked.
type RepricingLagConfig struct {
	Enabled         bool          `env:"REPRICING_LAG_ENABLED" envDefault:"true"`
	ShadowOnly      bool          `env:"REPRICING_LAG_SHADOW_ONLY" envDefault:"true"`
	MinLagCents     float64       `env:"REPRICING_LAG_MIN_CENTS" envDefault:"4" validate:"gt=0"`
	PeerMinCount    int           `env:"REPRICING_LAG_PEER_MIN_COUNT" envDefault:"3" validate:"gte=1,lte=50"`
	CheckWindowsCSV string        `env:"REPRICING_LAG_CHECK_WINDOWS" envDefault:"5m,30m,1h,6h,24h"`
	MaxAmbiguity    float64       `env:"REPRICING_LAG_MAX_AMBIGUITY" envDefault:"0.45" validate:"gte=0,lte=1"`
	OpenInterval    time.Duration `env:"REPRICING_LAG_OPEN_INTERVAL" envDefault:"30s" validate:"gt=0"`
	CloseGrace      time.Duration `env:"REPRICING_LAG_CLOSE_GRACE" envDefault:"3m" validate:"gt=0"`
}

// WalletCohortConfig — Strategy 7 (booster).
//
// v11.10-insider-prior raises MinSimilarity (0.5→0.65), drops
// MinEvents (3→2) because the new fresh-wallet burst branch can
// detect insider-like convergence with thin historical edge density,
// tightens cohort window (30m→20m) and raises MinCohortHits to 3.
//
// Fresh-wallet burst (new in v11.10): when ≥ FreshWalletMinBurst
// wallets first_seen_at ≤ FreshWalletMaxAge converge same-side on
// the same condition within ConvergenceWindow, the booster fires
// even when the historical co-trade edge density is weak. Backed by
// staged input — see walletcohort.Detector.Decide().
type WalletCohortConfig struct {
	Enabled             bool          `env:"WALLET_COHORT_ENABLED" envDefault:"true"`
	ShadowOnly          bool          `env:"WALLET_COHORT_SHADOW_ONLY" envDefault:"true"`
	MinSimilarity       float64       `env:"WALLET_COHORT_MIN_SIMILARITY" envDefault:"0.65" validate:"gt=0,lte=1"`
	MinEvents           int           `env:"WALLET_COHORT_MIN_EVENTS" envDefault:"2" validate:"gte=2,lte=50"`
	CoTradeWindow       time.Duration `env:"WALLET_COHORT_COTRADE_WINDOW" envDefault:"20m" validate:"gt=0"`
	UseFundingEdges     bool          `env:"WALLET_COHORT_USE_FUNDING_EDGES" envDefault:"false"`
	ConvergenceWindow   time.Duration `env:"WALLET_COHORT_CONVERGENCE_WINDOW" envDefault:"6h" validate:"gt=0"`
	MinCohortHits       int           `env:"WALLET_COHORT_MIN_COHORT_HITS" envDefault:"3" validate:"gte=2,lte=50"`
	FreshWalletMinBurst int           `env:"WALLET_COHORT_FRESH_WALLET_MIN_BURST" envDefault:"3" validate:"gte=2,lte=50"`
	FreshWalletMaxAge   time.Duration `env:"WALLET_COHORT_FRESH_WALLET_MAX_AGE" envDefault:"24h" validate:"gt=0"`
}

// ConflictResolveConfig — Strategy 8 (arbitration).
//
// v11.10-insider-prior raises dominance / quality floors; MMPenalty
// raised so MM-like signals are weighted down harder in arbitration.
type ConflictResolveConfig struct {
	Enabled       bool          `env:"CONFLICT_RESOLVE_ENABLED" envDefault:"true"`
	ShadowOnly    bool          `env:"CONFLICT_RESOLVE_SHADOW_ONLY" envDefault:"true"`
	Window        time.Duration `env:"CONFLICT_RESOLVE_WINDOW" envDefault:"20m" validate:"gt=0"`
	MinDominance  float64       `env:"CONFLICT_RESOLVE_MIN_DOMINANCE" envDefault:"1.8" validate:"gt=1"`
	MMPenalty     float64       `env:"CONFLICT_RESOLVE_MM_PENALTY" envDefault:"0.55" validate:"gte=0,lte=2"`
	MinQualitySum float64       `env:"CONFLICT_RESOLVE_MIN_QUALITY_SUM" envDefault:"1.25" validate:"gte=0"`
}

// RulesRiskConfig — Strategy 9 (safety/suppressor).
//
// v11.10-insider-prior lowers HighThreshold to 0.50 and Mid to 0.25
// to engage suppression sooner; the strengthened lexical scorer
// (wording risk, source specificity, procedural complexity) cleanly
// crosses these tiers on real-world ambiguous market questions.
// repricinglag + cheaptail blocking left ON so insider-prior never
// fires on oracle-sensitive markets without dual confirmation.
type RulesRiskConfig struct {
	Enabled            bool    `env:"RULES_RISK_ENABLED" envDefault:"true"`
	ShadowOnly         bool    `env:"RULES_RISK_SHADOW_ONLY" envDefault:"true"`
	HighThreshold      float64 `env:"RULES_RISK_HIGH_THRESHOLD" envDefault:"0.50" validate:"gt=0,lte=1"`
	MidThreshold       float64 `env:"RULES_RISK_MID_THRESHOLD" envDefault:"0.25" validate:"gt=0,lte=1"`
	HighCapSeverity    string  `env:"RULES_RISK_HIGH_CAP_SEVERITY" envDefault:"warning"`
	BlockRepricingHigh bool    `env:"RULES_RISK_BLOCK_REPRICING" envDefault:"true"`
	BlockCheapTailHigh bool    `env:"RULES_RISK_BLOCK_CHEAPTAIL" envDefault:"true"`
}

// CheapTailConfig — Strategy 4 (primary).
//
// v11.10-insider-prior widens the band from ultra-cheap (0.02–0.15)
// to a realistic insider-like long-shot band (0.03–0.25). Notional
// floor raised (1k→2.5k) so dust positions never fire; MinTrades
// dropped to 2 (single repeat already structurally meaningful when
// notional ≥ floor); AmbiguityCutoff tightened to 0.50.
type CheapTailConfig struct {
	Enabled         bool    `env:"CHEAPTAIL_ENABLED" envDefault:"true"`
	ShadowOnly      bool    `env:"CHEAPTAIL_SHADOW_ONLY" envDefault:"true"`
	MinProb         float64 `env:"CHEAPTAIL_MIN_PROB" envDefault:"0.03" validate:"gt=0,lt=1"`
	MaxProb         float64 `env:"CHEAPTAIL_MAX_PROB" envDefault:"0.25" validate:"gt=0,lt=1"`
	MinNotionalUSD  float64 `env:"CHEAPTAIL_MIN_NOTIONAL_USD" envDefault:"2500" validate:"gte=0"`
	MinTrades       int     `env:"CHEAPTAIL_MIN_TRADES" envDefault:"2" validate:"gte=1,lte=200"`
	RequireCatalyst bool    `env:"CHEAPTAIL_REQUIRE_CATALYST" envDefault:"true"`
	AmbiguityCutoff float64 `env:"CHEAPTAIL_AMBIGUITY_CUTOFF" envDefault:"0.50" validate:"gte=0,lte=1"`
}

// MarketLinksConfig — marketlinks.Builder worker.
type MarketLinksConfig struct {
	Enabled        bool          `env:"MARKETLINKS_ENABLED" envDefault:"true"`
	Interval       time.Duration `env:"MARKETLINKS_INTERVAL" envDefault:"15m" validate:"gt=0"`
	BatchSize      int           `env:"MARKETLINKS_BATCH_SIZE" envDefault:"150" validate:"gte=1,lte=2000"`
	LinkVersion    int           `env:"MARKETLINKS_LINK_VERSION" envDefault:"1" validate:"gte=1"`
	IncludeOpposed bool          `env:"MARKETLINKS_INCLUDE_OPPOSED" envDefault:"true"`
	MinConfidence  float64       `env:"MARKETLINKS_MIN_CONFIDENCE" envDefault:"0.40" validate:"gte=0,lte=1"`
}

// HolderSyncConfig — holdersync.Worker.
//
// v11.9 added SourceMode + RequireOpenInterest + RateLimitRPS knobs.
// v11.10-insider-prior:
//   - default SourceMode flips to "dataapi" (the verified Polymarket
//     /holders adapter; falls back to ErrNoSource if the wrapper is
//     missing, never produces fake data);
//   - both TopK fields are HARD-CAPPED at 20 because the upstream
//     /holders API is itself capped at 20. Asking for more than 20
//     would mislead holderdelta into claiming coverage it does not
//     have. Validators enforce the cap at boot (validate=lte=20).
type HolderSyncConfig struct {
	Enabled             bool          `env:"OWNERSHIP_SYNC_ENABLED" envDefault:"true"`
	WorkerEnabled       bool          `env:"HOLDERSYNC_WORKER_ENABLED" envDefault:"true"`
	SourceMode          string        `env:"HOLDERSYNC_SOURCE_MODE" envDefault:"dataapi"`
	Interval            time.Duration `env:"OWNERSHIP_SYNC_INTERVAL" envDefault:"20m" validate:"gt=0"`
	IntervalV2          time.Duration `env:"HOLDERSYNC_INTERVAL" envDefault:"5m" validate:"gt=0"`
	MaxMarkets          int           `env:"OWNERSHIP_SYNC_MAX_MARKETS" envDefault:"50" validate:"gte=1,lte=2000"`
	MaxMarketsV2        int           `env:"HOLDERSYNC_MAX_MARKETS" envDefault:"150" validate:"gte=1,lte=2000"`
	TopK                int           `env:"OWNERSHIP_SYNC_TOPK" envDefault:"20" validate:"gte=1,lte=20"`
	TopKV2              int           `env:"HOLDERSYNC_TOPK" envDefault:"20" validate:"gte=1,lte=20"`
	FetchTimeout        time.Duration `env:"OWNERSHIP_SYNC_TIMEOUT" envDefault:"15s" validate:"gt=0"`
	PerMarketTimeout    time.Duration `env:"HOLDERSYNC_PER_MARKET_TIMEOUT" envDefault:"3s" validate:"gt=0"`
	Concurrency         int           `env:"OWNERSHIP_SYNC_CONCURRENCY" envDefault:"3" validate:"gte=1,lte=32"`
	StaleAfter          time.Duration `env:"OWNERSHIP_SYNC_STALE_AFTER" envDefault:"6h" validate:"gt=0"`
	RateLimitRPS        float64       `env:"HOLDERSYNC_RATE_LIMIT_RPS" envDefault:"2" validate:"gte=0,lte=100"`
	RequireOpenInterest bool          `env:"HOLDERSYNC_REQUIRE_OPEN_INTEREST" envDefault:"true"`
}

// BookFeatureBarsConfig — v11.9 producer for polymarket_book_feature_bars.
// Producer itself is Phase E (requires WS Event depth field
// extension + aggregator). Knobs exist so the operator surface is
// stable when the producer lands.
//
// v11.10-insider-prior raises the default interval to 15s
// (matches the ТЗ insider-prior knobs; vacuum still uses 90s freshness
// window so a 15s producer keeps two fresh bars in-window) and lowers
// MaxMarkets to 150 to match HOLDERSYNC.
type BookFeatureBarsConfig struct {
	Enabled               bool          `env:"BOOK_FEATURE_BARS_ENABLED" envDefault:"true"`
	Interval              time.Duration `env:"BOOK_FEATURE_BARS_INTERVAL" envDefault:"15s" validate:"gt=0"`
	TopN                  int           `env:"BOOK_FEATURE_BARS_TOPN" envDefault:"5" validate:"gte=1,lte=50"`
	MaxMarkets            int           `env:"BOOK_FEATURE_BARS_MAX_MARKETS" envDefault:"150" validate:"gte=1,lte=5000"`
	RequireDepthForVacuum bool          `env:"BOOK_FEATURE_BARS_REQUIRE_DEPTH_FOR_VACUUM" envDefault:"true"`
	Retention             time.Duration `env:"BOOK_FEATURE_BARS_RETENTION" envDefault:"720h" validate:"gt=0"`
}

// ThesisLinesConfig — v11.9 background matrix that precomputes
// wallet directional exposure across linked markets. Hot-path
// reader is bounded by HOTPATH_MAX_LINKED_MARKETS + a per-query
// timeout.
//
// v11.10-insider-prior: Lookback MUST match ThesisAccumConfig
// LookbackLifetime (2160h = 90d). Drift between the two is a
// hot-path correctness bug — thesisaccum would consume a stale
// matrix or miss aligned exposure. Enforced by
// TestThesisLookbackConsistency in strategy_config_test.go.
// Interval shortened to 5m (was 10m) so the matrix stays fresh.
type ThesisLinesConfig struct {
	WorkerEnabled       bool          `env:"THESIS_LINES_WORKER_ENABLED" envDefault:"true"`
	Lookback            time.Duration `env:"THESIS_LINES_LOOKBACK" envDefault:"2160h" validate:"gt=0"`
	Interval            time.Duration `env:"THESIS_LINES_INTERVAL" envDefault:"5m" validate:"gt=0"`
	MaxEvents           int           `env:"THESIS_LINES_MAX_EVENTS" envDefault:"500" validate:"gte=1,lte=10000"`
	MaxWallets          int           `env:"THESIS_LINES_MAX_WALLETS" envDefault:"10000" validate:"gte=1,lte=1000000"`
	HotpathMaxLinked    int           `env:"THESIS_HOTPATH_MAX_LINKED_MARKETS" envDefault:"24" validate:"gte=2,lte=200"`
	HotpathQueryTimeout time.Duration `env:"THESIS_HOTPATH_QUERY_TIMEOUT" envDefault:"250ms" validate:"gt=0"`
}

// RiskScoreConfig — riskscore.Worker.
type RiskScoreConfig struct {
	Enabled      bool          `env:"RISKSCORE_ENABLED" envDefault:"true"`
	Interval     time.Duration `env:"RISKSCORE_INTERVAL" envDefault:"10m" validate:"gt=0"`
	BatchSize    int           `env:"RISKSCORE_BATCH_SIZE" envDefault:"150" validate:"gte=1,lte=5000"`
	ScoreVersion int           `env:"RISKSCORE_VERSION" envDefault:"1" validate:"gte=1"`
	RefreshOlder time.Duration `env:"RISKSCORE_REFRESH_OLDER_THAN" envDefault:"12h" validate:"gt=0"`
}

// RepricingWorkerConfig — repricing.Worker.
//
// v11.9 added the close-phase knobs: CloseEnabled flips on the
// real target+peer sampler, PriceSource picks between snapshots/
// trades/auto, MinPeerCount + MinLagCents drive the lag classifier.
//
// v11.10-insider-prior raises CloseAfter to 26h so the 24h horizon
// in RepricingLagConfig.CheckWindowsCSV has room to evaluate. Open
// interval tightened (30s) and lookback widened (30m) so a delayed
// event-page fetch never misses an annotation. PriceSource default
// flipped to "auto" (the worker picks snapshots → trades fallback).
type RepricingWorkerConfig struct {
	Enabled        bool          `env:"REPRICING_WORKER_ENABLED" envDefault:"true"`
	Interval       time.Duration `env:"REPRICING_WORKER_INTERVAL" envDefault:"30s" validate:"gt=0"`
	OpenLookback   time.Duration `env:"REPRICING_WORKER_OPEN_LOOKBACK" envDefault:"30m" validate:"gt=0"`
	MaxOpenWindows int           `env:"REPRICING_WORKER_MAX_OPEN_WINDOWS" envDefault:"1000" validate:"gte=1,lte=5000"`
	CloseAfter     time.Duration `env:"REPRICING_WORKER_CLOSE_AFTER" envDefault:"26h" validate:"gt=0"`
	CloseEnabled   bool          `env:"REPRICING_CLOSE_ENABLED" envDefault:"true"`
	MinPeerCount   int           `env:"REPRICING_MIN_PEER_COUNT" envDefault:"3" validate:"gte=1,lte=50"`
	MinLagCents    float64       `env:"REPRICING_MIN_LAG_CENTS" envDefault:"4" validate:"gt=0"`
	PriceSource    string        `env:"REPRICING_PRICE_SOURCE" envDefault:"auto"`
}

// WalletGraphConfig — walletgraph.Worker.
type WalletGraphConfig struct {
	Enabled            bool          `env:"WALLETGRAPH_ENABLED" envDefault:"true"`
	Interval           time.Duration `env:"WALLETGRAPH_INTERVAL" envDefault:"30m" validate:"gt=0"`
	CoTradeWindow      time.Duration `env:"WALLETGRAPH_COTRADE_WINDOW" envDefault:"20m" validate:"gt=0"`
	MinSharedEvents    int           `env:"WALLETGRAPH_MIN_SHARED_EVENTS" envDefault:"2" validate:"gte=2,lte=50"`
	BatchSize          int           `env:"WALLETGRAPH_BATCH_SIZE" envDefault:"5000" validate:"gte=100,lte=100000"`
	EdgeVersion        int           `env:"WALLETGRAPH_EDGE_VERSION" envDefault:"1" validate:"gte=1"`
	UseFundingProvider bool          `env:"WALLETGRAPH_USE_FUNDING_PROVIDER" envDefault:"false"`
}
