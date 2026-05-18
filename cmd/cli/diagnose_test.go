package main

import (
	"testing"
	"time"
)

// TestReadGatesFromEnv pins the env-derived gate defaults so a
// silent default-shift never sneaks past the watchtower app's loader
// (the diagnose tool's projection must agree with what production
// actually does).
func TestReadGatesFromEnv(t *testing.T) {
	t.Setenv("LIFECYCLE_ALERT_FROM_PCT", "65")
	t.Setenv("MARKET_MIN_AGE", "6h")
	t.Setenv("SINGLE_MIN_BASELINE_TRADES", "20")
	t.Setenv("SINGLE_MIN_BASELINE_NOTIONAL_USD", "2000")
	t.Setenv("BASELINE_MIN_READY_WINDOW", "3h")
	t.Setenv("ALERT_INFO_MIN_NOTIONAL_USD", "2500")
	t.Setenv("ALERT_INFO_MIN_ODDS", "2")
	t.Setenv("ALERT_INFO_MIN_MULTIPLIER", "25")
	t.Setenv("ALERT_WARNING_MIN_NOTIONAL_USD", "10000")
	t.Setenv("ALERT_WARNING_MIN_ODDS", "4")
	t.Setenv("ALERT_WARNING_MIN_MULTIPLIER", "50")
	t.Setenv("ALERT_CRITICAL_MIN_NOTIONAL_USD", "25000")
	t.Setenv("ALERT_CRITICAL_MIN_ODDS", "7")
	t.Setenv("ALERT_CRITICAL_MIN_MULTIPLIER", "100")

	g := readGatesFromEnv()
	if g.lifecycleFromPct != 65 {
		t.Errorf("lifecycleFromPct: %v", g.lifecycleFromPct)
	}
	if g.marketMinAge != 6*time.Hour {
		t.Errorf("marketMinAge: %v", g.marketMinAge)
	}
	if g.minBaselineTrades != 20 {
		t.Errorf("minBaselineTrades: %v", g.minBaselineTrades)
	}
	if g.minBaselineNotionalUSD != 2000 {
		t.Errorf("minBaselineNotionalUSD: %v", g.minBaselineNotionalUSD)
	}
	if g.minReadyWindow != 3*time.Hour {
		t.Errorf("minReadyWindow: %v", g.minReadyWindow)
	}
	if g.infoMinNotionalUSD != 2500 || g.infoMinOdds != 2 || g.infoMinMultiplier != 25 {
		t.Errorf("Info ladder mismatch: %+v", g)
	}
	if g.warnMinNotionalUSD != 10000 || g.warnMinOdds != 4 || g.warnMinMultiplier != 50 {
		t.Errorf("Warning ladder mismatch: %+v", g)
	}
	if g.critMinNotionalUSD != 25000 || g.critMinOdds != 7 || g.critMinMultiplier != 100 {
		t.Errorf("Critical ladder mismatch: %+v", g)
	}
}

// TestReadGatesFromEnv_FallbackDefaults confirms missing env vars
// resolve to compiled-in defaults rather than zero values.
func TestReadGatesFromEnv_FallbackDefaults(t *testing.T) {
	for _, k := range []string{
		"LIFECYCLE_ALERT_FROM_PCT", "MARKET_MIN_AGE",
		"SINGLE_MIN_BASELINE_TRADES", "SINGLE_MIN_BASELINE_NOTIONAL_USD",
		"BASELINE_MIN_READY_WINDOW",
		"ALERT_INFO_MIN_NOTIONAL_USD", "ALERT_INFO_MIN_ODDS", "ALERT_INFO_MIN_MULTIPLIER",
		"ALERT_WARNING_MIN_NOTIONAL_USD", "ALERT_WARNING_MIN_ODDS", "ALERT_WARNING_MIN_MULTIPLIER",
		"ALERT_CRITICAL_MIN_NOTIONAL_USD", "ALERT_CRITICAL_MIN_ODDS", "ALERT_CRITICAL_MIN_MULTIPLIER",
	} {
		t.Setenv(k, "")
	}
	g := readGatesFromEnv()
	if g.minBaselineTrades <= 0 || g.minBaselineNotionalUSD <= 0 || g.minReadyWindow <= 0 {
		t.Fatalf("readiness defaults must be positive: %+v", g)
	}
	if g.lifecycleFromPct <= 0 || g.marketMinAge <= 0 {
		t.Fatalf("lifecycle/age defaults must be positive: %+v", g)
	}
	if !(g.infoMinNotionalUSD < g.warnMinNotionalUSD && g.warnMinNotionalUSD < g.critMinNotionalUSD) {
		t.Fatalf("default notional ladder not monotonic: %+v", g)
	}
	if !(g.infoMinMultiplier < g.warnMinMultiplier && g.warnMinMultiplier < g.critMinMultiplier) {
		t.Fatalf("default multiplier ladder not monotonic: %+v", g)
	}
}

// TestEnvDuration_BadValueFallsBack pins that a malformed duration
// string (e.g. operator typo) silently falls back to the default —
// the diagnose tool must not crash on a stray "24" (no unit).
func TestEnvDuration_BadValueFallsBack(t *testing.T) {
	t.Setenv("MARKET_MIN_AGE", "24") // missing unit
	g := readGatesFromEnv()
	if g.marketMinAge == 0 {
		t.Fatal("bad duration should fall back to default, not zero")
	}
}

// TestEnvFloat_BadValueFallsBack pins the float fallback path.
func TestEnvFloat_BadValueFallsBack(t *testing.T) {
	t.Setenv("LIFECYCLE_ALERT_FROM_PCT", "not-a-number")
	g := readGatesFromEnv()
	if g.lifecycleFromPct <= 0 {
		t.Fatal("bad float should fall back to default, not zero")
	}
}
