package app

import (
	"testing"
	"time"
)

// loadConfigWithEnv resets the test's view of env vars to exactly the supplied
// map, runs LoadConfig, then restores the previous values.
func loadConfigWithEnv(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	keys := []string{
		"APP_ENV", "LOG_LEVEL", "METRICS_PORT", "SHUTDOWN_GRACE_PERIOD",
		"GAMMA_API_URL", "DATA_API_URL", "CLOB_API_URL",
		"POLYMARKET_HTTP_TIMEOUT", "POLYMARKET_USER_AGENT", "POLYMARKET_PUBLIC_BASE_URL",
		"RL_GAMMA_PER_SEC", "RL_GAMMA_BURST", "RL_DATAAPI_PER_SEC", "RL_DATAAPI_BURST",
		"DISCOVER_INTERVAL", "COLLECT_INTERVAL", "MAX_MARKETS", "ACTIVE_ONLY",
		"DISCOVER_ORDER", "COLLECT_CONCURRENCY",
		"AGG_BUCKET", "AGG_BASELINE_WINDOW", "AGG_RECENT_WINDOWS",
		"SINGLE_TRADE_MULTIPLIERS", "SINGLE_TRADE_ABSOLUTE_USD",
		"MIN_BASELINE_TRADES", "BASELINE_WINDOW", "BASELINE_MAX_SAMPLES",
		"HARD_ALERT_WINDOW", "HARD_ALERT_MIN_ANOMALOUS_TRADES",
		"HARD_ALERT_MIN_UNIQUE_TRADERS", "HARD_ALERT_MIN_TOTAL_NOTIONAL_USD",
		"HARD_ALERT_COOLDOWN",
		"ALERT_WEBHOOK_URL",
		"TELEGRAM_ENABLED", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"TELEGRAM_BASE_URL", "TELEGRAM_TIMEOUT",
		"GRAFANA_BASE_URL", "GRAFANA_DASH_UID", "GRAFANA_CONTEXT_WINDOW",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return LoadConfig()
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Application.Env != EnvDev {
		t.Errorf("env: want dev got %q", cfg.Application.Env)
	}
	if cfg.Application.MetricsPort != 9090 {
		t.Errorf("metrics port default: %d", cfg.Application.MetricsPort)
	}
	if cfg.Application.ShutdownGracePeriod != 15*time.Second {
		t.Errorf("grace period default: %s", cfg.Application.ShutdownGracePeriod)
	}
	if cfg.Polymarket.GammaURL == "" || cfg.Polymarket.DataAPIURL == "" {
		t.Errorf("default upstream URLs missing")
	}
	if cfg.Polymarket.PublicBaseURL != "https://polymarket.com" {
		t.Errorf("public base URL: %q", cfg.Polymarket.PublicBaseURL)
	}
	if got := cfg.Anomaly.Multipliers; len(got) != 3 || got[0] != 30 || got[1] != 100 || got[2] != 1000 {
		t.Errorf("multiplier ladder default: %v", got)
	}
	if got := cfg.Anomaly.AbsoluteUSDTiers; len(got) != 3 || got[0] != 3_000 || got[1] != 10_000 || got[2] != 100_000 {
		t.Errorf("absolute USD tiers default: %v", got)
	}
	if cfg.Anomaly.MinBaselineTrades != 20 {
		t.Errorf("MinBaselineTrades default: %d", cfg.Anomaly.MinBaselineTrades)
	}
	if cfg.Anomaly.HardAlertWindow != time.Hour {
		t.Errorf("HardAlertWindow default: %s", cfg.Anomaly.HardAlertWindow)
	}
	if cfg.Anomaly.HardAlertMinTrades != 5 || cfg.Anomaly.HardAlertMinWallets != 3 {
		t.Errorf("hard alert thresholds: trades=%d wallets=%d", cfg.Anomaly.HardAlertMinTrades, cfg.Anomaly.HardAlertMinWallets)
	}
	if cfg.Anomaly.HardAlertMinTotalUSD != 25_000 {
		t.Errorf("hard alert total USD default: %v", cfg.Anomaly.HardAlertMinTotalUSD)
	}
	if got := cfg.Aggregate.RecentWindows; len(got) != 2 || got[0] != 12*time.Hour || got[1] != 24*time.Hour {
		t.Errorf("recent windows default: %v", got)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, map[string]string{
		"APP_ENV":                       "prod",
		"METRICS_PORT":                  "8080",
		"AGG_RECENT_WINDOWS":            "1h,6h,12h",
		"SINGLE_TRADE_MULTIPLIERS":      "10,50",
		"SINGLE_TRADE_ABSOLUTE_USD":     "5000,25000",
		"HARD_ALERT_MIN_UNIQUE_TRADERS": "5",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Application.Env != EnvProd {
		t.Errorf("APP_ENV not applied: %q", cfg.Application.Env)
	}
	if cfg.Application.MetricsPort != 8080 {
		t.Errorf("port not applied: %d", cfg.Application.MetricsPort)
	}
	if got := cfg.Aggregate.RecentWindows; len(got) != 3 {
		t.Errorf("recent windows: %v", got)
	}
	if got := cfg.Anomaly.Multipliers; len(got) != 2 || got[0] != 10 || got[1] != 50 {
		t.Errorf("multipliers: %v", got)
	}
	if got := cfg.Anomaly.AbsoluteUSDTiers; len(got) != 2 || got[0] != 5_000 || got[1] != 25_000 {
		t.Errorf("absolute tiers: %v", got)
	}
	if cfg.Anomaly.HardAlertMinWallets != 5 {
		t.Errorf("hard alert wallets: %d", cfg.Anomaly.HardAlertMinWallets)
	}
}

func TestConfigRejectsInvalidEnv(t *testing.T) {
	_, err := loadConfigWithEnv(t, map[string]string{"APP_ENV": "staging"})
	if err == nil {
		t.Fatal("expected validation error for APP_ENV=staging")
	}
}

func TestConfigRejectsInvalidPort(t *testing.T) {
	_, err := loadConfigWithEnv(t, map[string]string{"METRICS_PORT": "0"})
	if err == nil {
		t.Fatal("expected validation error for METRICS_PORT=0")
	}
}

func TestConfigRejectsBadURL(t *testing.T) {
	_, err := loadConfigWithEnv(t, map[string]string{"GAMMA_API_URL": "not-a-url"})
	if err == nil {
		t.Fatal("expected validation error for bad URL")
	}
}
