package app

import (
	"os"
	"testing"

	"github.com/caarlos0/env/v11"
)

func TestStrategyConfig_DefaultsAreDisabled(t *testing.T) {
	clearEnvs := []string{
		"THESIS_ACCUM_ENABLED", "OWNERSHIP_V2_ENABLED", "CATALYST_WINDOW_ENABLED",
		"BOOK_VACUUM_ENABLED", "REPRICING_LAG_ENABLED", "WALLET_COHORT_ENABLED",
		"CONFLICT_RESOLVE_ENABLED", "RULES_RISK_ENABLED", "CHEAPTAIL_ENABLED",
		"MARKETLINKS_ENABLED", "OWNERSHIP_SYNC_ENABLED", "RISKSCORE_ENABLED",
		"REPRICING_WORKER_ENABLED", "WALLETGRAPH_ENABLED",
		"STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED",
	}
	for _, k := range clearEnvs {
		_ = os.Unsetenv(k)
	}

	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	if cfg.GlobalPromotionAllowed {
		t.Fatalf("GlobalPromotionAllowed must default false")
	}
	if cfg.ThesisAccum.Enabled || cfg.OwnershipV2.Enabled ||
		cfg.CatalystWindow.Enabled || cfg.BookVacuum.Enabled ||
		cfg.RepricingLag.Enabled || cfg.WalletCohort.Enabled ||
		cfg.ConflictResolve.Enabled || cfg.RulesRisk.Enabled ||
		cfg.CheapTail.Enabled {
		t.Fatalf("all detectors must default disabled; got: %+v", cfg)
	}
	if cfg.MarketLinks.Enabled || cfg.HolderSync.Enabled ||
		cfg.RiskScore.Enabled || cfg.Repricing.Enabled || cfg.WalletGraph.Enabled {
		t.Fatalf("all workers must default disabled; got: %+v", cfg)
	}
	if !cfg.ThesisAccum.ShadowOnly || !cfg.OwnershipV2.ShadowOnly ||
		!cfg.CatalystWindow.ShadowOnly || !cfg.BookVacuum.ShadowOnly ||
		!cfg.RepricingLag.ShadowOnly || !cfg.WalletCohort.ShadowOnly ||
		!cfg.ConflictResolve.ShadowOnly || !cfg.RulesRisk.ShadowOnly ||
		!cfg.CheapTail.ShadowOnly {
		t.Fatalf("all detectors must default shadow-only")
	}
}

// TestStrategyConfig_PromotionDefaultsMatchV11_6 pins the v11.7
// promotion-threshold defaults to the previously hardcoded values.
// A change here must be paired with a CLAUDE.md update.
func TestStrategyConfig_PromotionDefaultsMatchV11_6(t *testing.T) {
	for _, k := range []string{
		"STRATEGY_PROMOTION_MIN_SAMPLE",
		"STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS",
		"STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO",
		"STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY",
		"STRATEGY_PROMOTION_BYPASS_EXPLICIT",
		"STRATEGY_PROMOTION_REVIEW_INTERVAL",
		"STRATEGY_PROMOTION_REVIEW_LOOKBACK",
	} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	if cfg.PromotionMinSample != 50 {
		t.Fatalf("MinSample default drift: %d", cfg.PromotionMinSample)
	}
	if cfg.PromotionMinSignedMove6hCents != 1.5 {
		t.Fatalf("MinSignedMove6hCents default drift: %f", cfg.PromotionMinSignedMove6hCents)
	}
	if cfg.PromotionMaxReversal15mRatio != 0.5 {
		t.Fatalf("MaxReversal15mRatio default drift: %f", cfg.PromotionMaxReversal15mRatio)
	}
	if cfg.PromotionMaxAlertsPerDay != 40 {
		t.Fatalf("MaxAlertsPerDay default drift: %f", cfg.PromotionMaxAlertsPerDay)
	}
	if cfg.PromotionBypassExplicit {
		t.Fatalf("BypassExplicit must default false")
	}
}
