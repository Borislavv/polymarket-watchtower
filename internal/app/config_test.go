package app

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
)

func categoryFilter(blacklist []string) *category.Filter { return category.NewFilter(blacklist) }

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
		"ALERT_INFO_MIN_NOTIONAL_USD", "ALERT_INFO_MIN_ODDS", "ALERT_INFO_MIN_MULTIPLIER",
		"ALERT_WARNING_MIN_NOTIONAL_USD", "ALERT_WARNING_MIN_ODDS", "ALERT_WARNING_MIN_MULTIPLIER",
		"ALERT_CRITICAL_MIN_NOTIONAL_USD", "ALERT_CRITICAL_MIN_ODDS", "ALERT_CRITICAL_MIN_MULTIPLIER",
		"BASELINE_MIN_TRADE_USD",
		"SINGLE_MIN_BASELINE_TRADES", "SINGLE_MIN_BASELINE_NOTIONAL_USD",
		"BASELINE_WINDOW", "BASELINE_MAX_SAMPLES",
		"CLUSTER_WINDOW", "CLUSTER_MIN_ANOMALOUS_TRADES",
		"CLUSTER_MIN_UNIQUE_TRADERS", "CLUSTER_MIN_TOTAL_NOTIONAL_USD",
		"CLUSTER_COOLDOWN",
		"VOLUME_MULTIPLIERS", "VOLUME_MIN_NOTIONAL_USD", "VOLUME_MIN_TRADES", "VOLUME_COOLDOWN",
		"CATEGORY_BLACKLIST",
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
	if cfg.Anomaly.Mode != ModeSingleCluster {
		t.Errorf("default mode: %q", cfg.Anomaly.Mode)
	}
	if cfg.Anomaly.InfoMinNotionalUSD != 10_000 || cfg.Anomaly.InfoMinOdds != 3 || cfg.Anomaly.InfoMinMultiplier != 100 {
		t.Errorf("info tier defaults: notional=%v odds=%v mul=%v",
			cfg.Anomaly.InfoMinNotionalUSD, cfg.Anomaly.InfoMinOdds, cfg.Anomaly.InfoMinMultiplier)
	}
	if cfg.Anomaly.WarningMinNotionalUSD != 25_000 || cfg.Anomaly.WarningMinOdds != 5 || cfg.Anomaly.WarningMinMultiplier != 1_000 {
		t.Errorf("warning tier defaults: notional=%v odds=%v mul=%v",
			cfg.Anomaly.WarningMinNotionalUSD, cfg.Anomaly.WarningMinOdds, cfg.Anomaly.WarningMinMultiplier)
	}
	if cfg.Anomaly.CriticalMinNotionalUSD != 100_000 || cfg.Anomaly.CriticalMinOdds != 8 || cfg.Anomaly.CriticalMinMultiplier != 1_000 {
		t.Errorf("critical tier defaults: notional=%v odds=%v mul=%v",
			cfg.Anomaly.CriticalMinNotionalUSD, cfg.Anomaly.CriticalMinOdds, cfg.Anomaly.CriticalMinMultiplier)
	}
	if cfg.Anomaly.HardPromotionMinNotionalUSD != 100_000 || cfg.Anomaly.HardPromotionMinOdds != 8 || cfg.Anomaly.HardPromotionMinMultiplier != 1_000 {
		t.Errorf("hard-promotion defaults: notional=%v odds=%v mul=%v",
			cfg.Anomaly.HardPromotionMinNotionalUSD, cfg.Anomaly.HardPromotionMinOdds, cfg.Anomaly.HardPromotionMinMultiplier)
	}
	if cfg.Anomaly.AllowUnknownMarketLifecycle {
		t.Errorf("AllowUnknownMarketLifecycle default must be false (fail-closed)")
	}
	if cfg.Anomaly.BaselineMinTradeUSD != 50 {
		t.Errorf("baseline min trade: %v", cfg.Anomaly.BaselineMinTradeUSD)
	}
	if cfg.Anomaly.ClusterWindow != 30*time.Minute {
		t.Errorf("cluster window: %s", cfg.Anomaly.ClusterWindow)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, map[string]string{
		"ALERT_INFO_MIN_NOTIONAL_USD":   "1000",
		"ALERT_CRITICAL_MIN_MULTIPLIER": "5000",
		"BASELINE_MIN_TRADE_USD":        "100",
		"CLUSTER_MIN_UNIQUE_TRADERS":    "5",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Anomaly.InfoMinNotionalUSD != 1_000 {
		t.Errorf("info notional override: %v", cfg.Anomaly.InfoMinNotionalUSD)
	}
	if cfg.Anomaly.CriticalMinMultiplier != 5_000 {
		t.Errorf("critical mul override: %v", cfg.Anomaly.CriticalMinMultiplier)
	}
	if cfg.Anomaly.BaselineMinTradeUSD != 100 {
		t.Errorf("baseline min: %v", cfg.Anomaly.BaselineMinTradeUSD)
	}
	if cfg.Anomaly.ClusterMinWallets != 5 {
		t.Errorf("cluster wallets: %d", cfg.Anomaly.ClusterMinWallets)
	}
}

func TestConfigRejectsInvalidOdds(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"ALERT_INFO_MIN_ODDS": "0.5"}); err == nil {
		t.Fatal("expected validation error for odds < 1")
	}
}

func TestConfigCategoryBlacklistDefaults(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"sport", "nba", "nhl", "fifa", "uefa"}
	for _, w := range want {
		found := false
		for _, g := range cfg.CategoryFilter.Blacklist {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("default blacklist missing %q", w)
		}
	}
}

func TestConfigCategoryBlacklistOverride(t *testing.T) {
	cfg, err := loadConfigWithEnv(t, map[string]string{"CATEGORY_BLACKLIST": "weather, crypto"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	f := categoryFilter(cfg.CategoryFilter.Blacklist)
	if f.Allowed("", "Weather") || f.Allowed("", "Crypto Prices") {
		t.Error("override not applied")
	}
	if !f.Allowed("", "Politics") {
		t.Error("politics should pass")
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
	if _, err := loadConfigWithEnv(t, map[string]string{"ANOMALY_MODE": "rate"}); err == nil {
		t.Fatal("expected error for ANOMALY_MODE=rate")
	}
}

func TestConfigRejectsInvalidEnv(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"APP_ENV": "staging"}); err == nil {
		t.Fatal("expected error for APP_ENV=staging")
	}
}

func TestConfigRejectsInvalidPort(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"METRICS_PORT": "0"}); err == nil {
		t.Fatal("expected error for METRICS_PORT=0")
	}
}

func TestConfigRejectsBadURL(t *testing.T) {
	if _, err := loadConfigWithEnv(t, map[string]string{"GAMMA_API_URL": "not-a-url"}); err == nil {
		t.Fatal("expected error for bad URL")
	}
}
