// PART 8 (typed routing): hermetic tests for the v11.3 Router +
// RouterConfig. No network, no Prometheus registration.
//
// Pinned behaviours:
//   - signal surfaces route to the signal chat;
//   - admin surfaces route to the admin chat;
//   - admin missing/disabled does NOT fall back to the signal chat;
//   - legacy / blocked surfaces never dispatch;
//   - same chat is rejected by default;
//   - per-surface admin toggles suppress finely.
package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

const (
	testSignalChat = "-1001111111111"
	testAdminChat  = "-1002222222222"
)

// --- Fakes ---------------------------------------------------------------

type capturedSend struct {
	ChatID string
	Text   string
}

type recordingHTML struct {
	mu    sync.Mutex
	calls []capturedSend
	err   error
}

func (r *recordingHTML) SendHTML(_ context.Context, chatID, text string) (SendResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return SendResult{}, r.err
	}
	r.calls = append(r.calls, capturedSend{ChatID: chatID, Text: text})
	return SendResult{MessageID: int64(len(r.calls) + 100)}, nil
}

type recordingMetrics struct {
	mu         sync.Mutex
	route      []labels
	sent       []labels
	suppressed []labels
	sendFailed []labels
}

type labels [3]string

func (m *recordingMetrics) ObserveRoute(a, b, c string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.route = append(m.route, labels{a, b, c})
}
func (m *recordingMetrics) ObserveSent(a, b, c string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, labels{a, b, c})
}
func (m *recordingMetrics) ObserveSuppressed(a, b, c string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suppressed = append(m.suppressed, labels{a, b, c})
}
func (m *recordingMetrics) ObserveSendFailed(a, b, c string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendFailed = append(m.sendFailed, labels{a, b, c})
}

// --- Config validation ---------------------------------------------------

func TestRouterConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  RouterConfig
		ok   bool
	}{
		{
			name: "admin disabled + empty chat = ok",
			cfg:  RouterConfig{SignalEnabled: true, SignalChatID: testSignalChat},
			ok:   true,
		},
		{
			name: "admin enabled + empty chat = error",
			cfg:  RouterConfig{SignalEnabled: true, SignalChatID: testSignalChat, AdminEnabled: true},
			ok:   false,
		},
		{
			name: "admin chat equals signal chat = error",
			cfg: RouterConfig{
				SignalChatID: testSignalChat,
				AdminEnabled: true,
				AdminChatID:  testSignalChat,
			},
			ok: false,
		},
		{
			name: "same chat allowed with explicit override",
			cfg: RouterConfig{
				SignalChatID:  testSignalChat,
				AdminEnabled:  true,
				AdminChatID:   testSignalChat,
				AllowSameChat: true,
			},
			ok: true,
		},
		{
			name: "two distinct chats = ok",
			cfg: RouterConfig{
				SignalChatID: testSignalChat,
				AdminEnabled: true,
				AdminChatID:  testAdminChat,
			},
			ok: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.ok && err != nil {
				t.Fatalf("expected ok, got: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// --- Routing: signal surfaces ---------------------------------------------

func TestRoute_SignalSurfaces_RouteToSignalChat(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled: true,
		SignalChatID:  testSignalChat,
		AdminEnabled:  true,
		AdminChatID:   testAdminChat,
	}
	for _, s := range []Surface{
		SurfaceFlowAlert,
		SurfaceAccumulationAlert,
		SurfaceClusterAlert,
		SurfaceOwnershipAlert,
		SurfaceNewsIntelActionable,
		SurfaceOutcomeFollowup,
	} {
		dec := cfg.Route(s)
		if dec.Destination != DestinationSignal {
			t.Errorf("surface=%s: want destination=signal, got=%s", s, dec.Destination)
		}
		if dec.ChatID != testSignalChat {
			t.Errorf("surface=%s: want chat=%s, got=%s", s, testSignalChat, dec.ChatID)
		}
		if !dec.Enabled {
			t.Errorf("surface=%s: must be enabled", s)
		}
	}
}

// --- Routing: admin surfaces ---------------------------------------------

func TestRoute_AdminSignalQualityReport_RoutesToAdminChat(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              true,
		AdminChatID:               testAdminChat,
		AdminSignalQualityReports: true,
	}
	dec := cfg.Route(SurfaceSignalQualityReport)
	if dec.Destination != DestinationAdmin {
		t.Fatalf("want admin, got %s", dec.Destination)
	}
	if dec.ChatID != testAdminChat {
		t.Fatalf("want admin chat %s, got %s", testAdminChat, dec.ChatID)
	}
	if !dec.Enabled {
		t.Fatalf("admin signal quality must be enabled when toggle on")
	}
}

func TestRoute_AdminSignalQualityReport_SuppressedWhenToggleOff(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              true,
		AdminChatID:               testAdminChat,
		AdminSignalQualityReports: false, // toggle off
	}
	dec := cfg.Route(SurfaceSignalQualityReport)
	if dec.Destination != DestinationSuppressed {
		t.Fatalf("want suppressed, got %s", dec.Destination)
	}
	if dec.ChatID != "" {
		t.Fatalf("suppressed decision must NOT carry a chat id; got %q", dec.ChatID)
	}
	if dec.SuppressionReason != "admin_surface_disabled" {
		t.Errorf("want admin_surface_disabled, got %q", dec.SuppressionReason)
	}
}

// --- Admin DESTINATION never falls back to signal chat -------------------

func TestRoute_AdminDisabled_NeverFallsBackToSignalChat(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              false, // admin disabled entirely
		AdminChatID:               "",
		AdminSignalQualityReports: true,
	}
	dec := cfg.Route(SurfaceSignalQualityReport)
	if dec.Destination != DestinationSuppressed {
		t.Fatalf("admin disabled must suppress; got %s", dec.Destination)
	}
	if dec.ChatID == testSignalChat {
		t.Fatalf("admin report MUST NOT fall back to signal chat; got chat=%q", dec.ChatID)
	}
	if dec.SuppressionReason != "admin_disabled" {
		t.Errorf("want admin_disabled, got %q", dec.SuppressionReason)
	}
}

func TestRoute_AdminChatMissing_NeverFallsBackToSignalChat(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              true,
		AdminChatID:               "", // not configured
		AdminSignalQualityReports: true,
	}
	dec := cfg.Route(SurfaceSignalQualityReport)
	if dec.Destination != DestinationSuppressed {
		t.Fatalf("missing admin chat must suppress; got %s", dec.Destination)
	}
	if dec.ChatID == testSignalChat {
		t.Fatalf("admin report MUST NOT fall back to signal chat; got chat=%q", dec.ChatID)
	}
	if dec.SuppressionReason != "admin_chat_missing" {
		t.Errorf("want admin_chat_missing, got %q", dec.SuppressionReason)
	}
}

// --- Signal chat missing does not retarget to admin ----------------------

func TestRoute_SignalChatMissing_NeverRetargetsToAdmin(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled: true,
		SignalChatID:  "", // missing
		AdminEnabled:  true,
		AdminChatID:   testAdminChat,
	}
	dec := cfg.Route(SurfaceFlowAlert)
	if dec.Destination != DestinationSuppressed {
		t.Fatalf("missing signal chat must suppress; got %s", dec.Destination)
	}
	if dec.ChatID == testAdminChat {
		t.Fatalf("flow alert MUST NOT retarget to admin chat; got chat=%q", dec.ChatID)
	}
	if dec.SuppressionReason != "signal_chat_missing" {
		t.Errorf("want signal_chat_missing, got %q", dec.SuppressionReason)
	}
}

// --- Blocked surfaces ----------------------------------------------------

func TestRoute_BlockedSurfaces(t *testing.T) {
	cfg := RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              true,
		AdminChatID:               testAdminChat,
		AdminSignalQualityReports: true,
		AdminStats:                true,
		AdminStrategyScorecard:    true,
		AdminOperationalHealth:    true,
	}
	for _, s := range []Surface{
		SurfaceGenericMarketIntel,
		SurfaceDailyPoliticalIntel,
		SurfaceNoEdge,
		SurfacePredictionUpdate,
		SurfacePredictionStateTransition,
		SurfacePredictionBlocked,
	} {
		dec := cfg.Route(s)
		if dec.Destination != DestinationBlocked {
			t.Errorf("surface=%s: want destination=blocked, got %s", s, dec.Destination)
		}
		if dec.ChatID != "" {
			t.Errorf("blocked decision must not carry chat id; surface=%s chat=%q", s, dec.ChatID)
		}
	}
}

// --- Unknown surface treated as blocked ----------------------------------

func TestRoute_UnknownSurface_Blocked(t *testing.T) {
	cfg := RouterConfig{SignalEnabled: true, SignalChatID: testSignalChat}
	dec := cfg.Route(Surface("totally_unknown_surface"))
	if dec.Destination != DestinationBlocked {
		t.Fatalf("unknown surface must be blocked; got %s", dec.Destination)
	}
	if dec.SuppressionReason != "unknown_surface" {
		t.Errorf("want unknown_surface, got %q", dec.SuppressionReason)
	}
}

// --- Router.Send end-to-end ----------------------------------------------

func TestRouter_Send_DispatchesSignalAlert(t *testing.T) {
	inner := &recordingHTML{}
	met := &recordingMetrics{}
	r := NewRouter(RouterConfig{
		SignalEnabled: true,
		SignalChatID:  testSignalChat,
	}, inner, met)

	res, err := r.Send(context.Background(), Message{
		Surface: SurfaceFlowAlert,
		HTML:    "<b>CRITICAL: $25,000</b>",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.MessageID == 0 {
		t.Errorf("expected non-zero message id")
	}
	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 inner send, got %d", len(inner.calls))
	}
	if inner.calls[0].ChatID != testSignalChat {
		t.Errorf("want chat=%s, got %s", testSignalChat, inner.calls[0].ChatID)
	}
	if len(met.sent) != 1 || met.sent[0][1] != "signal" {
		t.Errorf("want sent counter labelled destination=signal; got %+v", met.sent)
	}
}

func TestRouter_Send_DispatchesAdminSignalQualityReport(t *testing.T) {
	inner := &recordingHTML{}
	met := &recordingMetrics{}
	r := NewRouter(RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              true,
		AdminChatID:               testAdminChat,
		AdminSignalQualityReports: true,
	}, inner, met)

	body := exactSignalQualityFixture()
	if _, err := r.Send(context.Background(), Message{
		Surface: SurfaceSignalQualityReport,
		HTML:    body,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 inner send, got %d", len(inner.calls))
	}
	if inner.calls[0].ChatID != testAdminChat {
		t.Fatalf("Signal quality MUST land on admin chat; got chat=%s", inner.calls[0].ChatID)
	}
	if inner.calls[0].ChatID == testSignalChat {
		t.Fatalf("Signal quality MUST NEVER land on the signal chat; chat=%s", inner.calls[0].ChatID)
	}
}

func TestRouter_Send_SuppressedAdminReportDoesNotDispatch(t *testing.T) {
	inner := &recordingHTML{}
	met := &recordingMetrics{}
	r := NewRouter(RouterConfig{
		SignalEnabled:             true,
		SignalChatID:              testSignalChat,
		AdminEnabled:              false, // admin off
		AdminChatID:               "",
		AdminSignalQualityReports: true,
	}, inner, met)

	if _, err := r.Send(context.Background(), Message{
		Surface: SurfaceSignalQualityReport,
		HTML:    exactSignalQualityFixture(),
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("admin disabled: inner must NOT be called; got %d", len(inner.calls))
	}
	if len(met.suppressed) != 1 {
		t.Fatalf("want exactly one suppressed counter; got %+v", met.suppressed)
	}
}

func TestRouter_Send_BlockedSurfaceDoesNotDispatch(t *testing.T) {
	inner := &recordingHTML{}
	met := &recordingMetrics{}
	r := NewRouter(RouterConfig{
		SignalEnabled: true, SignalChatID: testSignalChat,
		AdminEnabled: true, AdminChatID: testAdminChat,
	}, inner, met)

	if _, err := r.Send(context.Background(), Message{
		Surface: SurfacePredictionUpdate,
		HTML:    "<b>PREDICTION UPDATE</b> · blocked",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("legacy surface must NOT dispatch; got %d", len(inner.calls))
	}
}

func TestRouter_Send_TransportErrorIncrementsSendFailed(t *testing.T) {
	inner := &recordingHTML{err: errors.New("context deadline exceeded")}
	met := &recordingMetrics{}
	r := NewRouter(RouterConfig{
		SignalEnabled: true, SignalChatID: testSignalChat,
	}, inner, met)

	if _, err := r.Send(context.Background(), Message{
		Surface: SurfaceFlowAlert,
		HTML:    "<b>flow alert body</b>",
	}); err == nil {
		t.Fatalf("expected error")
	}
	if len(met.sendFailed) != 1 || met.sendFailed[0][2] != "timeout" {
		t.Errorf("want send_failed[reason=timeout]; got %+v", met.sendFailed)
	}
}

// --- The exact Signal quality fixture from the operator spec -------------

// exactSignalQualityFixture returns the body verbatim from the
// v11.3 PART 8 spec. The router test uses it to lock the route +
// destination contract on the exact wire shape.
func exactSignalQualityFixture() string {
	const body = `<b>Signal quality · Daily · 2026-05-20</b>

<b>Overview</b>
• total alerts sent: 74
• resolved: 0 (success 0 / failure 0)
• still pending: 74 (market not yet resolved)

⚠ Sample size is small; treat this as directional, not statistically stable.

<b>By alert kind</b>
• accumulation: 0/0 (n/a) — unresolved=40
• trade_anomaly: 0/0 (n/a) — unresolved=34

<b>By severity</b>
• info: 0/0 (n/a) — unresolved=53
• warning: 0/0 (n/a) — unresolved=16
• critical: 0/0 (n/a) — unresolved=5
`
	return body
}

// TestExactSignalQualityFixture_RoutesAdminOnly is the load-bearing
// regression. The fixture mirrors the operator-spec example
// verbatim and the test asserts the routing chain:
//
//   - surface=signal_quality_report + admin enabled → admin chat;
//   - the body NEVER touches the signal chat regardless of admin
//     state (suppressed > fallback);
//   - the guard tripwire ALSO refuses the body if a bypass tries
//     to send it directly to the signal chat.
func TestExactSignalQualityFixture_RoutesAdminOnly(t *testing.T) {
	body := exactSignalQualityFixture()
	if !strings.Contains(body, "Signal quality · Daily · 2026-05-20") {
		t.Fatalf("fixture drifted; body missing exact title")
	}

	// 1. Admin enabled + toggle on → routes to admin chat.
	{
		inner := &recordingHTML{}
		r := NewRouter(RouterConfig{
			SignalEnabled:             true,
			SignalChatID:              testSignalChat,
			AdminEnabled:              true,
			AdminChatID:               testAdminChat,
			AdminSignalQualityReports: true,
		}, inner, nil)
		if _, err := r.Send(context.Background(), Message{Surface: SurfaceSignalQualityReport, HTML: body}); err != nil {
			t.Fatalf("send: %v", err)
		}
		if len(inner.calls) != 1 || inner.calls[0].ChatID != testAdminChat {
			t.Fatalf("must route to admin chat=%s; got %+v", testAdminChat, inner.calls)
		}
	}
	// 2. Admin disabled → suppressed, NEVER falls back to signal.
	{
		inner := &recordingHTML{}
		r := NewRouter(RouterConfig{
			SignalEnabled: true, SignalChatID: testSignalChat,
			AdminEnabled: false,
		}, inner, nil)
		if _, err := r.Send(context.Background(), Message{Surface: SurfaceSignalQualityReport, HTML: body}); err != nil {
			t.Fatalf("send: %v", err)
		}
		if len(inner.calls) != 0 {
			t.Fatalf("admin disabled: inner must NOT be called; got %+v", inner.calls)
		}
	}
	// 3. Guard tripwire — bypass attempt directly to signal chat.
	{
		inner := &recordingHTML{}
		g := NewGuard(inner, GuardConfig{SignalChatID: testSignalChat}, nil)
		if _, err := g.SendHTML(context.Background(), testSignalChat, body); err != nil {
			t.Fatalf("guard.SendHTML: %v", err)
		}
		if len(inner.calls) != 0 {
			t.Fatalf("guard MUST refuse Signal quality body on signal chat; got %+v", inner.calls)
		}
	}
}
