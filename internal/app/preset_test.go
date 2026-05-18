package app

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadPresetEnv reads a key=value .env file and applies each line via
// t.Setenv so the test inherits the preset's effective config.
func loadPresetEnv(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k, v := strings.TrimSpace(line[:eq]), strings.TrimSpace(line[eq+1:])
		t.Setenv(k, v)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
}

// TestPresetBalancedIsStructurallySound asserts the shape invariants of the
// balanced preset rather than pinning exact values. Operators are expected
// to tune the preset within the same shape: tiers monotonic, lifecycle gate
// strictly between conservative and aggressive, baseline readiness gates
// non-zero. Code defaults and preset values are deliberately allowed to
// diverge — the preset is the operator's tuning surface.
func TestPresetBalancedIsStructurallySound(t *testing.T) {
	loadPresetEnv(t, filepath.Join("..", "..", "presets", "balanced.env"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	a := cfg.Anomaly
	// Severity ladders must be monotonically increasing.
	if !(a.InfoMinNotionalUSD < a.WarningMinNotionalUSD && a.WarningMinNotionalUSD < a.CriticalMinNotionalUSD) {
		t.Errorf("notional ladder not monotonic: %v / %v / %v",
			a.InfoMinNotionalUSD, a.WarningMinNotionalUSD, a.CriticalMinNotionalUSD)
	}
	if !(a.InfoMinOdds <= a.WarningMinOdds && a.WarningMinOdds <= a.CriticalMinOdds) {
		t.Errorf("odds ladder not monotonic: %v / %v / %v",
			a.InfoMinOdds, a.WarningMinOdds, a.CriticalMinOdds)
	}
	if !(a.InfoMinMultiplier <= a.WarningMinMultiplier && a.WarningMinMultiplier <= a.CriticalMinMultiplier) {
		t.Errorf("multiplier ladder not monotonic: %v / %v / %v",
			a.InfoMinMultiplier, a.WarningMinMultiplier, a.CriticalMinMultiplier)
	}
	// Balanced sits between conservative and aggressive on lifecycle gate.
	if a.LifecycleAlertFromPct <= 55 || a.LifecycleAlertFromPct >= 80 {
		t.Errorf("balanced lifecycle gate must sit between aggressive and conservative, got %v", a.LifecycleAlertFromPct)
	}
	// Hot threshold is always above the alert gate.
	if a.LifecycleHotFromPct < a.LifecycleAlertFromPct {
		t.Errorf("hot threshold %v must be >= alert threshold %v",
			a.LifecycleHotFromPct, a.LifecycleAlertFromPct)
	}
	// Baseline readiness gates must be set (otherwise the multiplier ladder
	// fires on noise).
	if a.SingleMinBaselineTrades <= 0 || a.SingleMinBaselineNotionalUSD <= 0 || a.BaselineMinReadySpan <= 0 {
		t.Errorf("baseline readiness gates must be positive: trades=%d notional=%v span=%s",
			a.SingleMinBaselineTrades, a.SingleMinBaselineNotionalUSD, a.BaselineMinReadySpan)
	}
}

func TestPresetConservativeIsStricter(t *testing.T) {
	loadPresetEnv(t, filepath.Join("..", "..", "presets", "conservative.env"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Anomaly.InfoMinNotionalUSD < 50_000 {
		t.Errorf("conservative notional too low: %v", cfg.Anomaly.InfoMinNotionalUSD)
	}
	if cfg.Anomaly.LifecycleAlertFromPct < 85 || cfg.Anomaly.LifecycleHotFromPct < 95 {
		t.Errorf("conservative lifecycle too lax: %v / %v",
			cfg.Anomaly.LifecycleAlertFromPct, cfg.Anomaly.LifecycleHotFromPct)
	}
	if cfg.Anomaly.MarketMinAge < 72*60*60*1_000_000_000 { // 72h in ns; avoids importing time
		t.Errorf("conservative MarketMinAge too low: %v", cfg.Anomaly.MarketMinAge)
	}
}

// TestPresetAggressiveIsLooser pins that the aggressive preset is in
// fact looser than the balanced preset on the load-bearing knobs. The
// exact values are an operator-tuned surface (re-derived from
// diagnose-alerts sweeps against the live DB); the invariant the test
// guards is "aggressive < balanced", not specific numerics.
func TestPresetAggressiveIsLooser(t *testing.T) {
	loadPresetEnv(t, filepath.Join("..", "..", "presets", "balanced.env"))
	bal, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig balanced: %v", err)
	}
	loadPresetEnv(t, filepath.Join("..", "..", "presets", "aggressive.env"))
	agg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig aggressive: %v", err)
	}
	if !(agg.Anomaly.InfoMinNotionalUSD < bal.Anomaly.InfoMinNotionalUSD) {
		t.Errorf("aggressive Info notional must be lower than balanced: %v vs %v",
			agg.Anomaly.InfoMinNotionalUSD, bal.Anomaly.InfoMinNotionalUSD)
	}
	if !(agg.Anomaly.LifecycleAlertFromPct < bal.Anomaly.LifecycleAlertFromPct) {
		t.Errorf("aggressive lifecycle gate must fire earlier than balanced: %v vs %v",
			agg.Anomaly.LifecycleAlertFromPct, bal.Anomaly.LifecycleAlertFromPct)
	}
	if !(agg.Anomaly.SingleMinBaselineTrades < bal.Anomaly.SingleMinBaselineTrades) {
		t.Errorf("aggressive baseline-trade floor must be lower than balanced: %d vs %d",
			agg.Anomaly.SingleMinBaselineTrades, bal.Anomaly.SingleMinBaselineTrades)
	}
	if !(agg.Anomaly.BaselineMinReadySpan < bal.Anomaly.BaselineMinReadySpan) {
		t.Errorf("aggressive baseline span must be shorter than balanced: %v vs %v",
			agg.Anomaly.BaselineMinReadySpan, bal.Anomaly.BaselineMinReadySpan)
	}
}

func TestAllPresetsRespectLifecycleGate(t *testing.T) {
	for _, name := range []string{"conservative", "balanced", "aggressive"} {
		t.Run(name, func(t *testing.T) {
			loadPresetEnv(t, filepath.Join("..", "..", "presets", name+".env"))
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Anomaly.LifecycleAlertFromPct <= 0 {
				t.Errorf("%s: lifecycle gate must be positive, got %v", name, cfg.Anomaly.LifecycleAlertFromPct)
			}
			if cfg.Anomaly.LifecycleHotFromPct < cfg.Anomaly.LifecycleAlertFromPct {
				t.Errorf("%s: hot threshold must be >= alert threshold", name)
			}
		})
	}
}

// TestPresetsHaveNoRemovedEnvVars guards against accidental reintroduction
// of env keys that the strategy refactor removed. If a preset sets one of
// these the binary will ignore it silently — better to fail the test loudly.
func TestPresetsHaveNoRemovedEnvVars(t *testing.T) {
	removed := []string{
		"BASELINE_MIN_TRADE_USD",
		"ALERT_HARD_A_MIN_NOTIONAL_USD", "ALERT_HARD_A_MIN_ODDS", "ALERT_HARD_A_MIN_MULTIPLIER",
		"ALERT_HARD_B_MIN_NOTIONAL_USD", "ALERT_HARD_B_MIN_ODDS", "ALERT_HARD_B_MIN_MULTIPLIER",
		"ALERT_HUGE_WHALE_MIN_NOTIONAL_USD", "ALERT_HUGE_WHALE_MIN_ODDS", "ALERT_HUGE_WHALE_MIN_MULTIPLIER",
		"ALERT_MEGA_WHALE_MIN_NOTIONAL_USD", "ALERT_MEGA_WHALE_MIN_ODDS", "ALERT_MEGA_WHALE_MIN_MULTIPLIER",
		"SUB_CLUSTER_WINDOW", "SUB_CLUSTER_MIN_TRADE_USD", "SUB_CLUSTER_MIN_ODDS",
		"SUB_CLUSTER_MIN_MULTIPLIER", "SUB_CLUSTER_MIN_UNIQUE_TRADERS",
		"SUB_CLUSTER_MIN_TOTAL_NOTIONAL_USD", "SUB_CLUSTER_COOLDOWN",
		"MARKET_KEYWORD_BLACKLIST",
		// v4 cleanup removals.
		"MAX_MARKETS",
		"AGG_BUCKET", "AGG_BASELINE_WINDOW", "AGG_RECENT_WINDOWS",
		"ANOMALY_MODE", "BASELINE_MAX_SAMPLES", "BASELINE_SOURCE",
		"STRATEGY_VERSION",
		"VOLUME_MULTIPLIERS", "VOLUME_MIN_NOTIONAL_USD", "VOLUME_MIN_TRADES", "VOLUME_COOLDOWN",
		"BACKFILL_CONCURRENCY",
	}
	for _, name := range []string{"conservative", "balanced", "aggressive"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "presets", name+".env")
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			defer func() { _ = f.Close() }()
			present := make(map[string]bool)
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				eq := strings.IndexByte(line, '=')
				if eq <= 0 {
					continue
				}
				present[strings.TrimSpace(line[:eq])] = true
			}
			for _, key := range removed {
				if present[key] {
					t.Errorf("preset %s reintroduces removed env var %s", name, key)
				}
			}
		})
	}
}
