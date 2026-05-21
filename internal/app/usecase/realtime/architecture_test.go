package realtime

import (
	"go/build"
	"strings"
	"testing"
)

// TestRealtimePackageHasNoTelegramOrAIDeps pins the v10.4 non-bypass
// rule: the realtime worker MUST NOT import telegram, AI, alerting,
// or any other "downstream" surface. The hybrid pipeline mandates
// that WS → realtime_work_queue is the only path; AI / Telegram /
// strategy/severity are handled by the alertsender + AI workers
// against the canonical polymarket_alerts pipeline.
//
// If this test starts failing, the v10.4 invariant has been broken
// and the spec must be re-read before merging.
func TestRealtimePackageHasNoTelegramOrAIDeps(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("import realtime package: %v", err)
	}
	forbidden := []string{
		"infra/telegram",
		"infra/ai",
		"infra/alerting",
		"usecase/alertsender",
		"domain/model/anomaly",
	}
	for _, imp := range pkg.Imports {
		for _, f := range forbidden {
			if strings.Contains(imp, f) {
				t.Errorf("realtime package must NOT import %q (found %q)", f, imp)
			}
		}
	}
}
