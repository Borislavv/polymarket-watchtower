package app

import (
	"testing"
	"time"
)

// loadConfigWithEnv resets the test's view of env vars to exactly the supplied
// map, runs LoadConfig, then restores the previous values. This keeps tests
// hermetic even when the parent shell has WATCHTOWER vars set.
func loadConfigWithEnv(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	keys := []string{
		"APP_ENV", "LOG_LEVEL", "METRICS_PORT", "SHUTDOWN_GRACE_PERIOD",
		"GAMMA_API_URL", "DATA_API_URL", "CLOB_API_URL",
		"POLYMARKET_HTTP_TIMEOUT", "POLYMARKET_USER_AGENT",
		"RL_GAMMA_PER_SEC", "RL_GAMMA_BURST", "RL_DATAAPI_PER_SEC", "RL_DATAAPI_BURST",
		"DISCOVER_INTERVAL", "COLLECT_INTERVAL", "MAX_MARKETS", "ACTIVE_ONLY",
		"DISCOVER_ORDER", "COLLECT_CONCURRENCY",
		"AGG_BUCKET", "AGG_BASELINE_WINDOW", "AGG_RECENT_WINDOWS",
		"ANOMALY_MULTIPLIERS", "ANOMALY_MIN_VOLUME_USD", "ANOMALY_MIN_TRADES", "ANOMALY_COOLDOWN",
		"ALERT_WEBHOOK_URL",
		"TELEGRAM_ENABLED", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"TELEGRAM_BASE_URL", "TELEGRAM_TIMEOUT",
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
	if got := cfg.Anomaly.Multipliers; len(got) != 3 || got[0] != 30 || got[1] != 100 || got[2] != 1000 {
		t.Errorf("multiplier ladder default: %v", got)
	}
	if got := cfg.Aggregate.RecentWindows; len(got) != 2 || got[0] != 12*time.Hour || got[1] != 24*time.Hour {
		t.Errorf("recent windows default: %v", got)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, map[string]string{
		"APP_ENV":             "prod",
		"METRICS_PORT":        "8080",
		"AGG_RECENT_WINDOWS":  "1h,6h,12h",
		"ANOMALY_MULTIPLIERS": "10,50",
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
	if got := cfg.Anomaly.Multipliers; len(got) != 2 {
		t.Errorf("multipliers: %v", got)
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
