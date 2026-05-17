package app

import (
	"testing"
	"time"
)

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
		"ANOMALY_MODE",
		"SINGLE_MIN_TRADE_USD", "SINGLE_MULTIPLIER_THRESHOLDS", "SINGLE_ODDS_THRESHOLDS",
		"SINGLE_MIN_BASELINE_TRADES", "SINGLE_MIN_BASELINE_NOTIONAL_USD",
		"BASELINE_WINDOW", "BASELINE_MAX_SAMPLES",
		"CLUSTER_WINDOW", "CLUSTER_MIN_ANOMALOUS_TRADES",
		"CLUSTER_MIN_UNIQUE_TRADERS", "CLUSTER_MIN_TOTAL_NOTIONAL_USD",
		"CLUSTER_COOLDOWN",
		"VOLUME_MULTIPLIERS", "VOLUME_MIN_NOTIONAL_USD", "VOLUME_MIN_TRADES", "VOLUME_COOLDOWN",
		"ALERT_WEBHOOK_URL",
		"TELEGRAM_ENABLED", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"TELEGRAM_BASE_URL", "TELEGRAM_TIMEOUT",
		"TELEGRAM_UPDATES_ENABLED", "TELEGRAM_UPDATES_INTERVAL",
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
	if cfg.Anomaly.Mode != ModeSingleCluster {
		t.Errorf("default mode: want single_cluster got %q", cfg.Anomaly.Mode)
	}
	if got := cfg.Anomaly.SingleMultiplierThresholds; len(got) != 3 || got[0] != 30 || got[2] != 1000 {
		t.Errorf("multipliers default: %v", got)
	}
	if got := cfg.Anomaly.SingleOddsThresholds; len(got) != 3 || got[0] != 3 || got[2] != 25 {
		t.Errorf("odds default: %v", got)
	}
	if cfg.Anomaly.SingleMinTradeUSD != 10_000 {
		t.Errorf("min trade default: %v", cfg.Anomaly.SingleMinTradeUSD)
	}
	if cfg.Anomaly.SingleMinBaselineTrades != 20 {
		t.Errorf("min baseline trades: %d", cfg.Anomaly.SingleMinBaselineTrades)
	}
	if cfg.Anomaly.SingleMinBaselineNotionalUSD != 1_000 {
		t.Errorf("min baseline notional: %v", cfg.Anomaly.SingleMinBaselineNotionalUSD)
	}
	if cfg.Anomaly.ClusterWindow != 30*time.Minute {
		t.Errorf("cluster window: %s", cfg.Anomaly.ClusterWindow)
	}
	if cfg.Anomaly.ClusterMinTrades != 3 || cfg.Anomaly.ClusterMinWallets != 2 {
		t.Errorf("cluster floors: trades=%d wallets=%d", cfg.Anomaly.ClusterMinTrades, cfg.Anomaly.ClusterMinWallets)
	}
	if cfg.Anomaly.ClusterMinTotalUSD != 30_000 {
		t.Errorf("cluster usd: %v", cfg.Anomaly.ClusterMinTotalUSD)
	}
}

func TestConfigVolumeMode(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, map[string]string{"ANOMALY_MODE": "volume"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Anomaly.Mode != ModeVolume {
		t.Errorf("mode: %q", cfg.Anomaly.Mode)
	}
}

func TestConfigRejectsInvalidMode(t *testing.T) {
	_, err := loadConfigWithEnv(t, map[string]string{"ANOMALY_MODE": "rate"})
	if err == nil {
		t.Fatal("expected validation error for ANOMALY_MODE=rate")
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, map[string]string{
		"APP_ENV":                      "prod",
		"METRICS_PORT":                 "8080",
		"SINGLE_MULTIPLIER_THRESHOLDS": "50",
		"SINGLE_ODDS_THRESHOLDS":       "2,5,20",
		"SINGLE_MIN_TRADE_USD":         "500",
		"CLUSTER_MIN_UNIQUE_TRADERS":   "5",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Application.Env != EnvProd || cfg.Application.MetricsPort != 8080 {
		t.Errorf("env/port: %s %d", cfg.Application.Env, cfg.Application.MetricsPort)
	}
	if got := cfg.Anomaly.SingleMultiplierThresholds; len(got) != 1 || got[0] != 50 {
		t.Errorf("multipliers: %v", got)
	}
	if got := cfg.Anomaly.SingleOddsThresholds; len(got) != 3 || got[2] != 20 {
		t.Errorf("odds: %v", got)
	}
	if cfg.Anomaly.SingleMinTradeUSD != 500 {
		t.Errorf("min trade usd: %v", cfg.Anomaly.SingleMinTradeUSD)
	}
	if cfg.Anomaly.ClusterMinWallets != 5 {
		t.Errorf("cluster wallets: %d", cfg.Anomaly.ClusterMinWallets)
	}
}

func TestConfigRejectsInvalidEnv(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"APP_ENV": "staging"}); err == nil {
		t.Fatal("expected validation error for APP_ENV=staging")
	}
}

func TestConfigRejectsInvalidPort(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"METRICS_PORT": "0"}); err == nil {
		t.Fatal("expected validation error for METRICS_PORT=0")
	}
}

func TestConfigRejectsBadURL(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"GAMMA_API_URL": "not-a-url"}); err == nil {
		t.Fatal("expected validation error for bad URL")
	}
}
