package app

import (
	"os"
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
)

// TestLegacyTelegramSurfaces_StayDisabledByDefault is the v11.6 PART 8
// pin. Re-enables of any of these env keys break the build (they are
// in the staleEnvKeys list) and the TELEGRAM_STATS default stays
// false. The test guards regressions where a future commit could
// accidentally re-introduce one of these surfaces.
func TestLegacyTelegramSurfaces_StayDisabledByDefault(t *testing.T) {
	for _, k := range []string{
		"TELEGRAM_STATS_ENABLED",
		"MARKET_INTEL_ENABLED",
		"DAILY_POLITICAL_INTEL_ENABLED",
		"MARKET_PREDICTION_CREATION_ENABLED",
		"MARKET_PREDICTION_EVOLUTION_ENABLED",
	} {
		_ = os.Unsetenv(k)
	}

	var stats StatsReportConfig
	if err := env.Parse(&stats); err != nil {
		t.Fatalf("env.Parse StatsReportConfig: %v", err)
	}
	if stats.Enabled {
		t.Fatalf("TELEGRAM_STATS_ENABLED must default false")
	}
}

// TestStaleEnvKeys_StillRejected ensures the legacy surface env keys
// remain in the stale list (boot fails loud if an operator sets one).
func TestStaleEnvKeys_StillRejected(t *testing.T) {
	required := []string{
		"MARKET_INTEL_ENABLED",
		"DAILY_POLITICAL_INTEL_ENABLED",
		"MARKET_PREDICTION_CREATION_ENABLED",
		"MARKET_PREDICTION_EVOLUTION_ENABLED",
		"PREDICTION_CREATION_ENABLED",
		"PREDICTION_EVOLUTION_ENABLED",
	}
	have := make(map[string]bool, len(staleEnvKeys))
	for _, k := range staleEnvKeys {
		have[k] = true
	}
	for _, k := range required {
		if !have[k] {
			t.Fatalf("legacy surface %q must remain in staleEnvKeys (else operators can silently re-enable it)\nstale set: %s",
				k, strings.Join(staleEnvKeys, ","))
		}
	}
}
