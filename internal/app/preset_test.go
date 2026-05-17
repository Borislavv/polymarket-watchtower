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

func TestPresetBalancedMatchesDefaults(t *testing.T) {
	loadPresetEnv(t, filepath.Join("..", "..", "presets", "balanced.env"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Anomaly.InfoMinNotionalUSD != 10_000 || cfg.Anomaly.LifecycleAlertFromPct != 75 {
		t.Fatalf("balanced preset diverged: notional=%v lifecycle=%v",
			cfg.Anomaly.InfoMinNotionalUSD, cfg.Anomaly.LifecycleAlertFromPct)
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

func TestPresetAggressiveIsLooser(t *testing.T) {
	loadPresetEnv(t, filepath.Join("..", "..", "presets", "aggressive.env"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Anomaly.InfoMinNotionalUSD >= 10_000 {
		t.Errorf("aggressive notional should be lower than balanced, got %v", cfg.Anomaly.InfoMinNotionalUSD)
	}
	if cfg.Anomaly.LifecycleAlertFromPct >= 75 {
		t.Errorf("aggressive lifecycle should fire earlier, got %v", cfg.Anomaly.LifecycleAlertFromPct)
	}
	if cfg.Anomaly.SingleMinBaselineTrades > 10 {
		t.Errorf("aggressive baseline-trade floor should be small, got %d", cfg.Anomaly.SingleMinBaselineTrades)
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
