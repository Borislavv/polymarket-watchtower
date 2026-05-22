// PART 7 / PART 8: hermetic tests for the v11.1 central Telegram
// suppression guard. No network. Verifies:
//   - every disabled noise surface is suppressed by the renderer body
//     it currently emits (statsreport / prediction evolution);
//   - flag-flipped allow-list lets the message through;
//   - real alert / news / signal-report bodies pass through.
package telegram

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

type captureSender struct {
	calls atomic.Int32
}

func (c *captureSender) SendHTML(_ context.Context, _ string, _ string) (SendResult, error) {
	c.calls.Add(1)
	return SendResult{MessageID: 42}, nil
}

// captureCounter records every WithLabelValues invocation. Backed by
// a real prometheus.CounterVec so Counter.Inc() is satisfied without
// reimplementing the full client_golang interface.
type captureCounter struct {
	vec *prometheus.CounterVec
	mu  []labelHit
}

type labelHit struct {
	labels []string
}

func newCaptureCounter() *captureCounter {
	return &captureCounter{
		vec: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "watchtower_test", Name: "guard_total",
			Help: "test only",
		}, []string{"surface", "reason"}),
	}
}

func (c *captureCounter) WithLabelValues(lvs ...string) prometheus.Counter {
	c.mu = append(c.mu, labelHit{labels: append([]string(nil), lvs...)})
	return c.vec.WithLabelValues(lvs...)
}

// Realistic body fixtures — copies of the strings the renderers emit
// today. If any renderer changes its header, this test fails before
// production sees the suppression bypass.
const (
	statsBody = `<b>Watchtower stats — uptime 8h0m</b>

<b>Markets</b>
• total: 4044
• active: 3928
• soft-deleted: 116
• purged: 0
`

	predictionUpdateBlocked = `<b>Type:</b> prediction_update

<b>PREDICTION UPDATE</b> · blocked

<b>Decision</b>
• stance: no new trade — wait for catalyst
• state: watching → blocked
• reason: active catalyst blocks repricing

<b>Catalyst</b>
• blocked until: 2026-12-15T18:00:00Z
• status: expected
`

	predictionUpdateActive = `<b>PREDICTION UPDATE</b> · active_catalyst

<b>Decision</b>
• stance: no new trade — wait for catalyst
• state: watching → active_catalyst
`

	realFlowAlert = `<b>CRITICAL: x250 · $25,000 · HOT · Will Trump win 2026?</b>

<b>Why</b>
• multiplier: 250× over market baseline median
• odds: 8.3
• lifecycle: 92%
`

	newsIntelBody = `<b>📰 News intel · ACTIONABLE</b>
<i>endorsement creates fresh repricing window</i>

1. <b>YES_up · 12h · conf 0.82</b>
<i>Will candidate win?</i>
Why it matters: endorser controls demographic that swings the runoff
`

	signalReportBody = `<b>SIGNAL REPORT · DAILY · 2026-05-22</b>

<b>Alerts</b>
• total: 23
• resolved correct: 14
• resolved wrong: 4
`
)

// Default v11.1 config: every surface disabled.
func disabledAll() GuardConfig {
	return GuardConfig{
		WatchtowerStatsEnabled:           false,
		PredictionUpdateEnabled:          false,
		PredictionStateTransitionEnabled: false,
		PredictionBlockedEnabled:         false,
	}
}

func enabledAll() GuardConfig {
	return GuardConfig{
		WatchtowerStatsEnabled:           true,
		PredictionUpdateEnabled:          true,
		PredictionStateTransitionEnabled: true,
		PredictionBlockedEnabled:         true,
	}
}

func TestGuard_StatsBody_SuppressedByDefault(t *testing.T) {
	inner := &captureSender{}
	g := NewGuard(inner, disabledAll(), nil)
	if _, err := g.SendHTML(context.Background(), "chat", statsBody); err != nil {
		t.Fatalf("SendHTML returned error on suppression: %v", err)
	}
	if inner.calls.Load() != 0 {
		t.Fatalf("Watchtower stats body MUST be suppressed by default; inner sender called %d times", inner.calls.Load())
	}
}

func TestGuard_PredictionUpdate_SuppressedByDefault(t *testing.T) {
	inner := &captureSender{}
	g := NewGuard(inner, disabledAll(), nil)
	for _, body := range []string{predictionUpdateBlocked, predictionUpdateActive} {
		if _, err := g.SendHTML(context.Background(), "chat", body); err != nil {
			t.Fatalf("SendHTML returned error on suppression: %v", err)
		}
	}
	if inner.calls.Load() != 0 {
		t.Fatalf("PREDICTION UPDATE bodies MUST be suppressed by default; inner sender called %d times", inner.calls.Load())
	}
}

func TestGuard_PredictionBlocked_HasItsOwnSurfaceLabel(t *testing.T) {
	inner := &captureSender{}
	cc := newCaptureCounter()
	g := NewGuard(inner, disabledAll(), cc)
	if _, err := g.SendHTML(context.Background(), "chat", predictionUpdateBlocked); err != nil {
		t.Fatalf("err: %v", err)
	}
	if inner.calls.Load() != 0 {
		t.Fatalf("blocked body must be suppressed")
	}
	if len(cc.mu) != 1 {
		t.Fatalf("expected exactly one counter hit, got %d", len(cc.mu))
	}
	if cc.mu[0].labels[0] != string(SurfacePredictionBlocked) {
		t.Errorf("blocked body should report surface=prediction_blocked, got %v", cc.mu[0].labels)
	}
}

func TestGuard_StateTransition_SuppressedSeparately(t *testing.T) {
	// Allow blocked + update, but deny state_transition. The active
	// PREDICTION UPDATE body carries "state: watching → active_catalyst"
	// so it should still be suppressed at the transition layer.
	cfg := GuardConfig{
		WatchtowerStatsEnabled:           false,
		PredictionUpdateEnabled:          true,
		PredictionStateTransitionEnabled: false,
		PredictionBlockedEnabled:         true,
	}
	inner := &captureSender{}
	cc := newCaptureCounter()
	g := NewGuard(inner, cfg, cc)
	if _, err := g.SendHTML(context.Background(), "chat", predictionUpdateActive); err != nil {
		t.Fatalf("err: %v", err)
	}
	if inner.calls.Load() != 0 {
		t.Fatalf("state-transition body MUST be suppressed when its flag is false (even if update is allowed)")
	}
	if len(cc.mu) != 1 || cc.mu[0].labels[0] != string(SurfacePredictionStateTransition) {
		t.Errorf("expected surface=prediction_state_transition counter, got %v", cc.mu)
	}
}

func TestGuard_AllowList_LetsBodyThrough(t *testing.T) {
	inner := &captureSender{}
	g := NewGuard(inner, enabledAll(), nil)
	// Statsbody + prediction update body both flow through unmodified.
	for _, body := range []string{statsBody, predictionUpdateBlocked, predictionUpdateActive} {
		if _, err := g.SendHTML(context.Background(), "chat", body); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	if inner.calls.Load() != 3 {
		t.Fatalf("allow-list should let all 3 bodies through; got %d", inner.calls.Load())
	}
}

func TestGuard_RealFlowAlert_PassesThroughByDefault(t *testing.T) {
	inner := &captureSender{}
	g := NewGuard(inner, disabledAll(), nil)
	if _, err := g.SendHTML(context.Background(), "chat", realFlowAlert); err != nil {
		t.Fatalf("err: %v", err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("real flow alert MUST pass through the guard; got %d", inner.calls.Load())
	}
}

func TestGuard_NewsIntel_PassesThroughByDefault(t *testing.T) {
	inner := &captureSender{}
	g := NewGuard(inner, disabledAll(), nil)
	if _, err := g.SendHTML(context.Background(), "chat", newsIntelBody); err != nil {
		t.Fatalf("err: %v", err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("hourly news intel MUST pass through the guard; got %d", inner.calls.Load())
	}
}

func TestGuard_SignalReport_PassesThroughByDefault(t *testing.T) {
	inner := &captureSender{}
	g := NewGuard(inner, disabledAll(), nil)
	if _, err := g.SendHTML(context.Background(), "chat", signalReportBody); err != nil {
		t.Fatalf("err: %v", err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("signal report MUST pass through the guard; got %d", inner.calls.Load())
	}
}

func TestGuard_Classify_StatsHeader(t *testing.T) {
	g := NewGuard(&captureSender{}, disabledAll(), nil)
	surface, reason, blocked := g.Classify(statsBody)
	if !blocked || surface != SurfaceWatchtowerStats || reason != "config_disabled" {
		t.Fatalf("expected (watchtower_stats, config_disabled, true), got (%q, %q, %v)", surface, reason, blocked)
	}
}

func TestGuard_Classify_AsciiArrowAlsoMatches(t *testing.T) {
	// The renderer uses U+2192 (→); operator-authored manual sends may
	// use "->" — both must be recognised.
	body := `<b>PREDICTION UPDATE</b> · active_catalyst
• state: watching -> active_catalyst
`
	g := NewGuard(&captureSender{}, disabledAll(), nil)
	surface, _, blocked := g.Classify(body)
	if !blocked {
		t.Fatalf("ascii-arrow state transition must be detected")
	}
	if surface != SurfacePredictionStateTransition {
		t.Errorf("expected SurfacePredictionStateTransition, got %q", surface)
	}
}

func TestGuard_Classify_PassesUnknownBody(t *testing.T) {
	body := "<b>Some other system message</b>\nbody body body"
	g := NewGuard(&captureSender{}, disabledAll(), nil)
	if surface, _, blocked := g.Classify(body); blocked {
		t.Fatalf("unknown body must not be suppressed; got surface=%q", surface)
	}
}
