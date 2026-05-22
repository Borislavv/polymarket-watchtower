// guard.go — central Telegram suppression guard (v11.1).
//
// A second line of defense against accidental Telegram delivery for
// disabled surfaces. The guard wraps a Sender (the bot) and inspects
// the outgoing HTML body for known disabled-surface markers. When a
// marker is recognised AND the matching config flag is set to false,
// the call is suppressed silently and the
// watchtower_telegram_suppressed_total{surface,reason} counter is
// bumped.
//
// Even when every config flag defaults to false (the v11.x state), an
// operator who flips one knob upstream still gets the surface. The
// guard is intentionally conservative: it triggers ONLY on the strict
// header substrings each renderer emits, NOT on incidental mentions
// of those words inside a market title or an annotation body.
//
// The guard never blocks alert/flow/news messages — those don't carry
// any of the matched markers.
package telegram

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Counter is the minimal Prometheus surface the guard needs. The
// production wiring passes *metrics.Metrics.TelegramSuppressed.
// Telegram package can't import metrics (would cycle), so we accept a
// raw CounterVec.
type Counter interface {
	WithLabelValues(lvs ...string) prometheus.Counter
}

// Surfaces names every Telegram surface the guard can recognise.
// Adding a new disabled surface = adding a new entry here, a new
// marker rule, and a new field on GuardConfig.
type Surface string

const (
	// SurfaceWatchtowerStats is the periodic "Watchtower stats —
	// uptime …" heartbeat. Disabled by default in v11.x.
	SurfaceWatchtowerStats Surface = "watchtower_stats"

	// SurfacePredictionUpdate is the "PREDICTION UPDATE · <state>"
	// body the evolution worker emits on any state transition.
	SurfacePredictionUpdate Surface = "prediction_update"

	// SurfacePredictionStateTransition is the more specific
	// "state: A → B" line that appears inside a PREDICTION UPDATE
	// body — separated so operators can keep AI-refresh updates
	// while killing state-transition spam.
	SurfacePredictionStateTransition Surface = "prediction_state_transition"

	// SurfacePredictionBlocked is the "blocked" / "active catalyst
	// blocks repricing" / "blocked until" variant of the prediction
	// update body. Sub-set of SurfacePredictionUpdate.
	SurfacePredictionBlocked Surface = "prediction_blocked"
)

// GuardConfig is the per-surface allow/deny matrix. true = the
// surface is permitted; false = the surface is suppressed and a
// metric is emitted. Defaults are false (every disabled surface
// listed here stays disabled).
//
// v11.3: the guard is a defense-in-depth tripwire BELOW the typed
// Router. Every active caller now passes through the Router which
// already enforces routing. The guard catches:
//
//   - any future caller that bypasses the Router and calls
//     bot.SendHTML / guardedHTML.SendHTML directly with a legacy
//     body (Watchtower stats / PREDICTION UPDATE / blocked /
//     state transition);
//   - the specific case of an admin-only body (e.g. "Signal
//     quality · ...") accidentally landing on the signal chat —
//     gated by SignalChatID below.
//
// SignalChatID is optional. When set, the guard knows which chat
// is the signal feed and suppresses any admin-marker body
// targeting it.
type GuardConfig struct {
	WatchtowerStatsEnabled           bool
	PredictionUpdateEnabled          bool
	PredictionStateTransitionEnabled bool
	PredictionBlockedEnabled         bool

	// v11.3: when non-empty, the guard refuses to deliver
	// admin-marker bodies (Signal quality, strategy scorecard,
	// etc.) to this specific chat id — the signal chat. The check
	// is independent of the per-surface toggles above.
	SignalChatID string
}

// AllDisabled reports whether every surface this guard knows about is
// suppressed. When true the guard can short-circuit the marker scan
// for any message that matches ANY rule.
func (c GuardConfig) AllDisabled() bool {
	return !c.WatchtowerStatsEnabled &&
		!c.PredictionUpdateEnabled &&
		!c.PredictionStateTransitionEnabled &&
		!c.PredictionBlockedEnabled
}

// HTMLSender is the low-level transport interface the guard wraps.
// *Bot satisfies it. The typed Sender interface (router.go) is the
// preferred public surface for new callers; HTMLSender is used by
// the outcome workers that need a direct chat-id send (EditMessage
// / SetMessageReaction live on *Bot, not on Sender).
type HTMLSender interface {
	SendHTML(ctx context.Context, chatID, text string) (SendResult, error)
}

// Guard intercepts SendHTML calls, inspects the body, and short-
// circuits any message that matches a disabled surface. Metric
// emission is best-effort; nil counters are tolerated.
type Guard struct {
	inner HTMLSender
	cfg   GuardConfig
	met   Counter
}

// NewGuard wires a Guard around an existing HTMLSender. Passing a
// fully-permissive cfg makes the guard a no-op (the marker scan is
// still skipped, since AllDisabled() is false and no rules trigger).
func NewGuard(inner HTMLSender, cfg GuardConfig, met Counter) *Guard {
	return &Guard{inner: inner, cfg: cfg, met: met}
}

// SendHTML applies the suppression rules and delegates when permitted.
// On suppression, returns a zero SendResult and a nil error so the
// caller treats the message as quietly accepted — this matches the
// "fail closed but silent" rule for disabled surfaces.
//
// v11.3: the additional admin-on-signal tripwire fires when an
// admin-marker body (e.g. "Signal quality · ...") is targeted at
// the configured SignalChatID. This catches a future caller that
// bypasses the typed Router and tries to send admin telemetry
// directly to the signal chat.
func (g *Guard) SendHTML(ctx context.Context, chatID, text string) (SendResult, error) {
	if surface, reason, blocked := g.classifyForChat(chatID, text); blocked {
		if g.met != nil {
			g.met.WithLabelValues(string(surface), reason).Inc()
		}
		return SendResult{}, nil
	}
	return g.inner.SendHTML(ctx, chatID, text)
}

// classifyForChat extends Classify with the chat-id-aware
// admin-on-signal tripwire. Returns (surface, reason, true) when
// the message should be suppressed.
func (g *Guard) classifyForChat(chatID, text string) (Surface, string, bool) {
	if g.cfg.SignalChatID != "" && chatID == g.cfg.SignalChatID && isAdminMarkerBody(text) {
		return SurfaceSignalQualityReport, "admin_marker_on_signal_chat", true
	}
	return g.Classify(text)
}

// isAdminMarkerBody recognises admin-channel body markers. The set
// is the inverse of the operator-facing signal feed: any body that
// is structurally telemetry must NOT reach the signal chat. The
// router enforces this through typed surfaces; this is the
// transport-level last line of defense.
func isAdminMarkerBody(text string) bool {
	for _, marker := range adminMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// adminMarkers are the body fragments that uniquely identify a
// v11.3 admin surface. Kept short and exact — a generic word like
// "quality" would risk false positives on real flow alerts.
var adminMarkers = []string{
	"<b>Signal quality · ",
	"Signal quality · Daily ",
	"Signal quality · Weekly ",
	"Signal quality · Monthly ",
	"Signal quality · Quarterly ",
	"Signal quality · Yearly ",
}

// Classify is exported for tests. Returns (surface, reason, true) when
// the message should be suppressed, ("", "", false) otherwise.
//
// Rules (order matters; first match wins):
//
//  1. "Watchtower stats — " header  → SurfaceWatchtowerStats
//  2. "PREDICTION UPDATE"           → SurfacePredictionUpdate
//  3. "state: <X> → <Y>" line       → SurfacePredictionStateTransition
//  4. "active catalyst blocks repricing"
//     OR "blocked until: "          → SurfacePredictionBlocked
//
// Each rule consults its config flag; when the flag is true the
// surface is allowed through.
func (g *Guard) Classify(text string) (Surface, string, bool) {
	if g.cfg.AllDisabled() {
		// Hot path: cheap substring check, then full marker scan.
	}

	// 1. Watchtower stats — the renderer always opens with this exact
	// header (statsreport.Format → "Watchtower stats — uptime ...").
	if strings.Contains(text, "<b>Watchtower stats — uptime") || strings.Contains(text, "Watchtower stats — uptime") {
		if !g.cfg.WatchtowerStatsEnabled {
			return SurfaceWatchtowerStats, "config_disabled", true
		}
	}

	// 2-4. Prediction surfaces — checked in specificity order so the
	// finer-grained labels are emitted on the metric even when the
	// generic "PREDICTION UPDATE" header is absent. v11.2: the
	// fragments are unique enough to the deleted renderer that a
	// false positive on a real flow alert is structurally
	// impossible (no live renderer emits these phrases).
	if isPredictionBlocked(text) {
		if !g.cfg.PredictionBlockedEnabled {
			return SurfacePredictionBlocked, "config_disabled", true
		}
	}
	if isPredictionStateTransition(text) {
		if !g.cfg.PredictionStateTransitionEnabled {
			return SurfacePredictionStateTransition, "config_disabled", true
		}
	}
	if strings.Contains(text, "PREDICTION UPDATE") {
		if !g.cfg.PredictionUpdateEnabled {
			return SurfacePredictionUpdate, "config_disabled", true
		}
	}

	return "", "", false
}

// isPredictionBlocked detects the "blocked" PREDICTION UPDATE variant.
// Either the new-state token "blocked" appears in the header line, or
// the body carries the explicit reason / catalyst-eta markers.
func isPredictionBlocked(text string) bool {
	if strings.Contains(text, "PREDICTION UPDATE</b> · blocked") ||
		strings.Contains(text, "PREDICTION UPDATE · blocked") {
		return true
	}
	if strings.Contains(text, "active catalyst blocks repricing") {
		return true
	}
	if strings.Contains(text, "• blocked until:") {
		return true
	}
	return false
}

// isPredictionStateTransition matches the "state: A → B" body line.
// Accepts both the arrow form (renderer emits U+2192) and the ascii
// fallback "->", because operator-authored manual sends may use the
// latter.
func isPredictionStateTransition(text string) bool {
	if strings.Contains(text, "state: ") && (strings.Contains(text, " → ") || strings.Contains(text, " -> ")) {
		return true
	}
	if strings.Contains(text, "state_transition:") {
		return true
	}
	return false
}
