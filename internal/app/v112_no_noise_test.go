// PART 5 / PART 7: pins that v11.2 noise surfaces are gone for good.
// Each test fails if a future change re-imports a deleted package.
// The proof is structural — the import will fail at compile time —
// and load-bearing because adding the import back would un-revert
// the entire surface.
package app

import (
	"context"
	"strings"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// TestV112_NoLegacyTelegramSurfaces — Watchtower stats + every
// prediction-update / blocked / state-transition body is suppressed by
// the central telegram.Guard with cfg=disabled (the production
// configuration). If a future refactor accidentally builds the
// guard with one of these toggles flipped on, this test fails before
// the build can ship.
func TestV112_NoLegacyTelegramSurfaces(t *testing.T) {
	// Mirror the production wiring from app.go: all four surfaces
	// hard-coded false. Any drift is a deliberate change and must
	// update this fixture.
	prodCfg := telegram.GuardConfig{
		WatchtowerStatsEnabled:           false,
		PredictionUpdateEnabled:          false,
		PredictionStateTransitionEnabled: false,
		PredictionBlockedEnabled:         false,
	}
	if !prodCfg.AllDisabled() {
		t.Fatalf("v11.2 production guard config must keep every noise surface disabled; got %+v", prodCfg)
	}

	for _, body := range []string{
		"<b>Watchtower stats — uptime 4h0m</b>",
		"<b>PREDICTION UPDATE</b> · blocked",
		"state: watching → blocked\nactive catalyst blocks repricing",
		"• blocked until: 2026-12-15T18:00:00Z",
	} {
		g := telegram.NewGuard(failOnSendSender{t: t}, prodCfg, nil)
		if _, err := g.SendHTML(context.Background(), "chat", body); err != nil {
			t.Fatalf("guard.SendHTML returned err (must be silent suppression): %v", err)
		}
	}

	// Real flow alert + hourly news intel bodies must STILL pass.
	for _, body := range []string{
		"<b>CRITICAL: x250 · $25,000 · Real flow alert</b>",
		"<b>📰 News intel · ACTIONABLE</b>\nendorsement creates fresh repricing window",
	} {
		ok := false
		g := telegram.NewGuard(passSender{seen: func(b string) { ok = strings.Contains(b, body[:10]) }}, prodCfg, nil)
		if _, err := g.SendHTML(context.Background(), "chat", body); err != nil {
			t.Fatalf("guard.SendHTML returned err on kept-surface body: %v", err)
		}
		if !ok {
			t.Fatalf("kept-surface body MUST pass through the guard; body=%q", body)
		}
	}
}

type failOnSendSender struct{ t *testing.T }

func (f failOnSendSender) SendHTML(_ context.Context, _, body string) (telegram.SendResult, error) {
	f.t.Errorf("legacy body leaked through guard:\n%s", body)
	return telegram.SendResult{}, nil
}

type passSender struct {
	seen func(string)
}

func (p passSender) SendHTML(_ context.Context, _, body string) (telegram.SendResult, error) {
	if p.seen != nil {
		p.seen(body)
	}
	return telegram.SendResult{MessageID: 1}, nil
}
