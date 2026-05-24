package app

import (
	"os"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-playground/validator"
)

// TestStrategyConfig_DefaultsShadowOnly pins the v11.10-insider-prior
// contract: detectors are ENABLED-by-default but SHADOW_ONLY=true so
// they write to polymarket_strategy_shadow_decisions and never to
// polymarket_alerts. Promotion stays globally false. This is the
// only safe path that exercises the staged-input + bus pipeline in
// production without any Telegram leakage.
func TestStrategyConfig_DefaultsShadowOnly(t *testing.T) {
	clearEnvs := []string{
		"THESIS_ACCUM_ENABLED", "OWNERSHIP_V2_ENABLED", "CATALYST_WINDOW_ENABLED",
		"BOOK_VACUUM_ENABLED", "REPRICING_LAG_ENABLED", "WALLET_COHORT_ENABLED",
		"CONFLICT_RESOLVE_ENABLED", "RULES_RISK_ENABLED", "CHEAPTAIL_ENABLED",
		"MARKETLINKS_ENABLED", "OWNERSHIP_SYNC_ENABLED", "HOLDERSYNC_WORKER_ENABLED",
		"RISKSCORE_ENABLED", "REPRICING_WORKER_ENABLED", "WALLETGRAPH_ENABLED",
		"THESIS_LINES_WORKER_ENABLED", "BOOK_FEATURE_BARS_ENABLED",
		"STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED",
		"THESIS_ACCUM_SHADOW_ONLY", "OWNERSHIP_V2_SHADOW_ONLY", "CATALYST_WINDOW_SHADOW_ONLY",
		"BOOK_VACUUM_SHADOW_ONLY", "REPRICING_LAG_SHADOW_ONLY", "WALLET_COHORT_SHADOW_ONLY",
		"CONFLICT_RESOLVE_SHADOW_ONLY", "RULES_RISK_SHADOW_ONLY", "CHEAPTAIL_SHADOW_ONLY",
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
	if !cfg.ThesisAccum.Enabled || !cfg.OwnershipV2.Enabled ||
		!cfg.CatalystWindow.Enabled || !cfg.BookVacuum.Enabled ||
		!cfg.RepricingLag.Enabled || !cfg.WalletCohort.Enabled ||
		!cfg.ConflictResolve.Enabled || !cfg.RulesRisk.Enabled ||
		!cfg.CheapTail.Enabled {
		t.Fatalf("v11.10-insider-prior: all detectors must default ENABLED; got: %+v", cfg)
	}
	if !cfg.MarketLinks.Enabled || !cfg.HolderSync.WorkerEnabled ||
		!cfg.RiskScore.Enabled || !cfg.Repricing.Enabled || !cfg.WalletGraph.Enabled ||
		!cfg.ThesisLines.WorkerEnabled || !cfg.BookFeatureBars.Enabled {
		t.Fatalf("v11.10-insider-prior: all workers must default ENABLED; got: %+v", cfg)
	}
	if !cfg.ThesisAccum.ShadowOnly || !cfg.OwnershipV2.ShadowOnly ||
		!cfg.CatalystWindow.ShadowOnly || !cfg.BookVacuum.ShadowOnly ||
		!cfg.RepricingLag.ShadowOnly || !cfg.WalletCohort.ShadowOnly ||
		!cfg.ConflictResolve.ShadowOnly || !cfg.RulesRisk.ShadowOnly ||
		!cfg.CheapTail.ShadowOnly {
		t.Fatalf("v11.10-insider-prior: every detector must default shadow-only")
	}
}

// TestStrategyConfig_PromotionDefaultsV11_10 pins the v11.10-insider-prior
// promotion-review threshold defaults. Tighter than v11.6.
func TestStrategyConfig_PromotionDefaultsV11_10(t *testing.T) {
	for _, k := range []string{
		"STRATEGY_PROMOTION_MIN_SAMPLE",
		"STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS",
		"STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO",
		"STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY",
		"STRATEGY_PROMOTION_BYPASS_EXPLICIT",
		"STRATEGY_PROMOTION_FORCE_DISABLE",
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
	if cfg.PromotionMinSignedMove6hCents != 2.0 {
		t.Fatalf("MinSignedMove6hCents default drift: %f (expected 2.0)", cfg.PromotionMinSignedMove6hCents)
	}
	if cfg.PromotionMaxReversal15mRatio != 0.35 {
		t.Fatalf("MaxReversal15mRatio default drift: %f (expected 0.35)", cfg.PromotionMaxReversal15mRatio)
	}
	if cfg.PromotionMaxAlertsPerDay != 15 {
		t.Fatalf("MaxAlertsPerDay default drift: %f (expected 15)", cfg.PromotionMaxAlertsPerDay)
	}
	if cfg.PromotionBypassExplicit {
		t.Fatalf("PromotionBypassExplicit must default false")
	}
	if cfg.PromotionForceDisable {
		t.Fatalf("PromotionForceDisable must default false")
	}
}

// TestStrategyConfig_HoldersyncTopKCappedAt20 verifies the upstream
// /holders API cap is enforced at the config validator level. Asking
// for more than 20 would mislead holderdelta into claiming coverage
// it does not have.
func TestStrategyConfig_HoldersyncTopKCappedAt20(t *testing.T) {
	for _, k := range []string{"HOLDERSYNC_TOPK", "OWNERSHIP_SYNC_TOPK"} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	if cfg.HolderSync.TopKV2 != 20 {
		t.Fatalf("HOLDERSYNC_TOPK default must be 20 (upstream cap); got %d", cfg.HolderSync.TopKV2)
	}
	if cfg.HolderSync.TopK != 20 {
		t.Fatalf("OWNERSHIP_SYNC_TOPK default must be 20 (upstream cap); got %d", cfg.HolderSync.TopK)
	}
}

// TestStrategyConfig_HoldersyncTopKAbove20Rejected proves the validator
// rejects HOLDERSYNC_TOPK=21. The cap is structural (Polymarket
// /holders returns at most 20 entries) so an operator override would
// be a bug, not a tuning knob.
func TestStrategyConfig_HoldersyncTopKAbove20Rejected(t *testing.T) {
	t.Setenv("HOLDERSYNC_TOPK", "21")
	defer func() { _ = os.Unsetenv("HOLDERSYNC_TOPK") }()
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	// caarlos0/env doesn't validate; do the assertion using a manual
	// re-validate via go-playground/validator (the same path
	// Config.MustParse uses).
	if err := validator.New().Struct(&cfg); err == nil {
		t.Fatalf("expected validator rejection for HOLDERSYNC_TOPK=21; got nil")
	}
}

// TestThesisLookbackConsistency pins the THESIS_ACCUM lifetime / THESIS_LINES
// worker lookback equivalence. Drift between the two is a hot-path
// correctness bug — thesisaccum reads a stale or under-covered matrix.
func TestThesisLookbackConsistency(t *testing.T) {
	for _, k := range []string{"THESIS_ACCUM_LOOKBACK_LIFETIME", "THESIS_LINES_LOOKBACK"} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	if cfg.ThesisAccum.LookbackLifetime != cfg.ThesisLines.Lookback {
		t.Fatalf("THESIS_ACCUM_LOOKBACK_LIFETIME (%s) must equal THESIS_LINES_LOOKBACK (%s)",
			cfg.ThesisAccum.LookbackLifetime, cfg.ThesisLines.Lookback)
	}
}

// TestPromotionBypassExplicit_IsForceDisable proves the historical
// "bypass" name does NOT enable a bypass. When set, the gate denies
// every promotion request regardless of review state. The canonical
// alias (STRATEGY_PROMOTION_FORCE_DISABLE) and the legacy key both
// share this force-disable semantic via logical OR in
// strategy_phase_b.go.
func TestPromotionBypassExplicit_IsForceDisable(t *testing.T) {
	for _, k := range []string{"STRATEGY_PROMOTION_BYPASS_EXPLICIT", "STRATEGY_PROMOTION_FORCE_DISABLE"} {
		_ = os.Unsetenv(k)
	}
	t.Setenv("STRATEGY_PROMOTION_BYPASS_EXPLICIT", "true")
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	// The combined kill-switch (logical OR in strategy_phase_b.go).
	killSwitch := cfg.PromotionForceDisable || cfg.PromotionBypassExplicit
	if !killSwitch {
		t.Fatalf("BYPASS_EXPLICIT=true must close the gate (force-disable); kill_switch=%v", killSwitch)
	}
}

// TestPromotionForceDisable_CanonicalAlias proves the canonical
// STRATEGY_PROMOTION_FORCE_DISABLE alias closes the gate the same way.
func TestPromotionForceDisable_CanonicalAlias(t *testing.T) {
	for _, k := range []string{"STRATEGY_PROMOTION_BYPASS_EXPLICIT", "STRATEGY_PROMOTION_FORCE_DISABLE"} {
		_ = os.Unsetenv(k)
	}
	t.Setenv("STRATEGY_PROMOTION_FORCE_DISABLE", "true")
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	killSwitch := cfg.PromotionForceDisable || cfg.PromotionBypassExplicit
	if !killSwitch {
		t.Fatalf("FORCE_DISABLE=true must close the gate; kill_switch=%v", killSwitch)
	}
}

// TestRepricingLagWindows_FullHorizonCoverage pins the v11.10-insider-prior
// requirement: 5m,30m,1h,6h,24h must all parse and the worker
// CloseAfter must be ≥ longest window (otherwise the 24h horizon
// can never be evaluated).
func TestRepricingLagWindows_FullHorizonCoverage(t *testing.T) {
	for _, k := range []string{
		"REPRICING_LAG_CHECK_WINDOWS",
		"REPRICING_WORKER_CLOSE_AFTER",
	} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	windows, err := cfg.RepricingLag.ParsedCheckWindows()
	if err != nil {
		t.Fatalf("ParsedCheckWindows: %v", err)
	}
	required := []time.Duration{
		5 * time.Minute, 30 * time.Minute,
		1 * time.Hour, 6 * time.Hour, 24 * time.Hour,
	}
	if len(windows) != len(required) {
		t.Fatalf("expected %d windows; got %d (%v)", len(required), len(windows), windows)
	}
	for i, w := range required {
		if windows[i] != w {
			t.Fatalf("window[%d]: expected %v; got %v", i, w, windows[i])
		}
	}
	// CloseAfter must cover the longest window.
	longest := windows[len(windows)-1]
	if cfg.Repricing.CloseAfter < longest {
		t.Fatalf("REPRICING_WORKER_CLOSE_AFTER (%v) must be ≥ longest CHECK_WINDOWS entry (%v)",
			cfg.Repricing.CloseAfter, longest)
	}
}

// TestRepricingLagWindows_ParseRejectsBadToken proves parser hardening.
func TestRepricingLagWindows_ParseRejectsBadToken(t *testing.T) {
	cfg := RepricingLagConfig{CheckWindowsCSV: "5m,not-a-duration,1h"}
	_, err := cfg.ParsedCheckWindows()
	if err == nil {
		t.Fatalf("expected parse error for bad token; got nil")
	}
}

// ---- v11.12-insider-prior cross-field invariants ---------------

// TestStrategyConfig_ValidateInvariants_ThesisLookbackMustMatch
// pins the invariant: hot-path thesisaccum reads what the
// thesislines worker writes; lookback divergence is a bug.
func TestStrategyConfig_ValidateInvariants_ThesisLookbackMustMatch(t *testing.T) {
	for _, k := range []string{"THESIS_ACCUM_LOOKBACK_LIFETIME", "THESIS_LINES_LOOKBACK"} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	if err := cfg.validateInvariants(); err != nil {
		t.Fatalf("aligned defaults must pass; got %v", err)
	}
	cfg.ThesisAccum.LookbackLifetime = 720 * time.Hour
	cfg.ThesisLines.Lookback = 2160 * time.Hour
	if err := cfg.validateInvariants(); err == nil {
		t.Fatalf("expected error on lookback divergence; got nil")
	}
}

// TestStrategyConfig_ValidateInvariants_24hHorizonRequires26hCloseAfter
// pins: REPRICING_LAG 24h horizon ⇒ REPRICING_WORKER_CLOSE_AFTER ≥ 26h.
func TestStrategyConfig_ValidateInvariants_24hHorizonRequires26hCloseAfter(t *testing.T) {
	for _, k := range []string{
		"REPRICING_LAG_CHECK_WINDOWS",
		"REPRICING_WORKER_CLOSE_AFTER",
		"THESIS_ACCUM_LOOKBACK_LIFETIME",
		"THESIS_LINES_LOOKBACK",
	} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	cfg.Repricing.CloseAfter = 12 * time.Hour
	if err := cfg.validateInvariants(); err == nil {
		t.Fatalf("expected error for CLOSE_AFTER=12h with 24h horizon; got nil")
	}
	cfg.Repricing.CloseAfter = 26 * time.Hour
	if err := cfg.validateInvariants(); err != nil {
		t.Fatalf("26h CLOSE_AFTER must pass; got %v", err)
	}
}

// TestStrategyConfig_ValidateInvariants_HolderSyncTopK20Cap pins the
// cross-field cap at the validateInvariants layer (struct-tag already
// guards lte=20 — this gives a clearer error).
func TestStrategyConfig_ValidateInvariants_HolderSyncTopK20Cap(t *testing.T) {
	for _, k := range []string{"HOLDERSYNC_TOPK", "OWNERSHIP_SYNC_TOPK", "THESIS_ACCUM_LOOKBACK_LIFETIME", "THESIS_LINES_LOOKBACK"} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	// Bypass struct-tag validator by mutating directly.
	cfg.HolderSync.TopKV2 = 25
	if err := cfg.validateInvariants(); err == nil {
		t.Fatalf("expected error for TOPK=25; got nil")
	}
	cfg.HolderSync.TopKV2 = 20
	cfg.HolderSync.TopK = 30
	if err := cfg.validateInvariants(); err == nil {
		t.Fatalf("expected error for legacy OWNERSHIP_SYNC_TOPK=30; got nil")
	}
}

// TestStrategyConfig_ValidateInvariants_BookVacuumRequiresProducer
// pins: BOOK_VACUUM_ENABLED=true requires BOOK_FEATURE_BARS_ENABLED=true.
func TestStrategyConfig_ValidateInvariants_BookVacuumRequiresProducer(t *testing.T) {
	for _, k := range []string{
		"BOOK_VACUUM_ENABLED", "BOOK_FEATURE_BARS_ENABLED",
		"THESIS_ACCUM_LOOKBACK_LIFETIME", "THESIS_LINES_LOOKBACK",
	} {
		_ = os.Unsetenv(k)
	}
	var cfg StrategyConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse: %v", err)
	}
	cfg.BookVacuum.Enabled = true
	cfg.BookFeatureBars.Enabled = false
	if err := cfg.validateInvariants(); err == nil {
		t.Fatalf("expected error: bookvacuum needs the bookbars producer")
	}
	cfg.BookFeatureBars.Enabled = true
	if err := cfg.validateInvariants(); err != nil {
		t.Fatalf("both enabled must pass; got %v", err)
	}
}
