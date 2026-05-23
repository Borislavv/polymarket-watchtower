// router.go — typed Telegram surface routing (v11.3).
//
// Watchtower runs TWO Telegram chats:
//
//   - SIGNAL chat (TELEGRAM_CHAT_ID): customer-facing high-signal
//     feed. ONLY real flow alerts and actionable hourly news
//     intelligence may land here.
//   - ADMIN  chat (TELEGRAM_ADMIN_CHAT_ID): internal telemetry —
//     signal-quality reports, stats, scorecards, operational
//     health, budget reports, suppression summaries. Operator-only.
//
// The Router is the single typed surface every caller talks to.
// Each Message carries an explicit Surface; the Router maps
// Surface → Destination → ChatID via deterministic rules (no body
// inspection, no fallback to the wrong chat).
//
// Hard guarantees:
//   - Admin-destination messages NEVER fall back to the signal chat
//     when the admin chat is missing or disabled.
//   - Legacy / no-edge / generic-market-intel / prediction
//     surfaces are routed to DestinationBlocked and silently
//     dropped (with a metric).
//   - The Router never inspects message text. The text-marker
//     guard (guard.go) is a defense-in-depth tripwire for any
//     legacy call site that still lacks a typed Surface; in
//     production every active caller passes one explicitly.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// --- Surface taxonomy -----------------------------------------------------
//
// Existing constants in guard.go (kept for back-compat):
//   SurfaceWatchtowerStats
//   SurfacePredictionUpdate
//   SurfacePredictionStateTransition
//   SurfacePredictionBlocked
//
// New v11.3 surfaces below. The set is intentionally finite — every
// active caller picks one of these and the Router keys destination
// off it. Adding a new surface = adding the constant + extending
// Route() + extending guard.go's Classify() if it carries a
// recognisable text marker.

const (
	// --- Signal-destination surfaces -----------------------------
	// Real flow alerts. The alertsender uses the granular
	// constants when the Finding kind is known, falling back to
	// SurfaceFlowAlert for the generic single-trade case.
	SurfaceFlowAlert           Surface = "flow_alert"
	SurfaceAccumulationAlert   Surface = "accumulation_alert"
	SurfaceClusterAlert        Surface = "cluster_alert"
	SurfaceOwnershipAlert      Surface = "ownership_alert"
	SurfaceNewsIntelActionable Surface = "news_intel_actionable"
	// Follow-up body the outcome-AI worker sends when
	// EditMessageText is unavailable. Treated as signal because it
	// is the resolved-verdict annotation on a previously-sent flow
	// alert.
	SurfaceOutcomeFollowup Surface = "outcome_followup"

	// --- Admin-destination surfaces ------------------------------
	SurfaceSignalQualityReport Surface = "signal_quality_report"
	SurfaceStrategyScorecard   Surface = "strategy_scorecard"
	SurfaceOperationalHealth   Surface = "operational_health"
	SurfaceBudgetReport        Surface = "budget_report"
	SurfaceSuppressionReport   Surface = "suppression_report"
	// SurfaceMarketCloseReview is the v11.4 learning-loop body
	// (one per resolved market). Admin destination only; gated
	// by AdminSignalQualityReports today since both reports
	// share the same operator-facing audience. A dedicated
	// toggle can be carved out later if needed.
	SurfaceMarketCloseReview Surface = "market_close_review"

	// --- Blocked-destination surfaces (legacy) -------------------
	// These mirror the v10.x surfaces the v11.x cleanup retired.
	// Routing them to Blocked makes the suppression contract
	// explicit at the caller level and the metric label stable.
	SurfaceGenericMarketIntel  Surface = "generic_market_intel"
	SurfaceDailyPoliticalIntel Surface = "daily_political_intel"
	SurfaceNoEdge              Surface = "no_edge"
)

// IsSignalSurface reports whether the surface is part of the
// customer-facing feed. Used by tests + Route().
func IsSignalSurface(s Surface) bool {
	switch s {
	case SurfaceFlowAlert,
		SurfaceAccumulationAlert,
		SurfaceClusterAlert,
		SurfaceOwnershipAlert,
		SurfaceNewsIntelActionable,
		SurfaceOutcomeFollowup:
		return true
	}
	return false
}

// IsAdminSurface reports whether the surface is internal telemetry.
func IsAdminSurface(s Surface) bool {
	switch s {
	case SurfaceSignalQualityReport,
		SurfaceWatchtowerStats,
		SurfaceStrategyScorecard,
		SurfaceOperationalHealth,
		SurfaceBudgetReport,
		SurfaceSuppressionReport,
		SurfaceMarketCloseReview:
		return true
	}
	return false
}

// IsBlockedSurface reports whether the surface is legacy / no-edge
// and must never reach either chat.
func IsBlockedSurface(s Surface) bool {
	switch s {
	case SurfaceGenericMarketIntel,
		SurfaceDailyPoliticalIntel,
		SurfaceNoEdge,
		SurfacePredictionUpdate,
		SurfacePredictionStateTransition,
		SurfacePredictionBlocked:
		return true
	}
	return false
}

// --- Destination ----------------------------------------------------------

type Destination string

const (
	DestinationSignal     Destination = "signal"
	DestinationAdmin      Destination = "admin"
	DestinationBlocked    Destination = "blocked"
	DestinationSuppressed Destination = "suppressed" // valid surface, disabled by config
)

// --- Decision -------------------------------------------------------------

// RouteDecision is the per-message routing verdict. Returned by
// RouterConfig.Route. Callers consult Enabled + ChatID to decide
// whether the underlying transport gets the call; on a blocked /
// suppressed decision the metric is bumped and the message is
// silently dropped.
type RouteDecision struct {
	Surface           Surface
	Destination       Destination
	ChatID            string
	Enabled           bool
	SuppressionReason string
}

// --- Router config --------------------------------------------------------

// RouterConfig is the typed routing matrix. Defaults are everything
// signal-enabled and everything admin-disabled — the operator opts
// in to admin surfaces explicitly.
type RouterConfig struct {
	// Signal chat
	SignalEnabled bool
	SignalChatID  string

	// Admin chat
	AdminEnabled  bool
	AdminChatID   string
	AllowSameChat bool

	// Per-surface admin toggles
	AdminSignalQualityReports bool
	AdminStats                bool
	AdminStrategyScorecard    bool
	AdminOperationalHealth    bool
	AdminBudgetReports        bool
	AdminSuppressionReports   bool
}

// Validate enforces the documented config invariants:
//   - admin enabled requires non-empty admin chat
//   - signal and admin chats may not be equal unless explicitly
//     allowed (default: deny — protects the production "two chats"
//     setup against accidental misconfig)
func (c RouterConfig) Validate() error {
	if c.AdminEnabled && strings.TrimSpace(c.AdminChatID) == "" {
		return errors.New("telegram router: TELEGRAM_ADMIN_ENABLED=true requires TELEGRAM_ADMIN_CHAT_ID")
	}
	if c.AdminChatID != "" && c.SignalChatID != "" && c.AdminChatID == c.SignalChatID && !c.AllowSameChat {
		return fmt.Errorf("telegram router: TELEGRAM_ADMIN_CHAT_ID (%s) must not equal TELEGRAM_CHAT_ID; set TELEGRAM_ALLOW_SAME_CHAT_FOR_ADMIN=true to override", c.AdminChatID)
	}
	return nil
}

// Route resolves a Surface to a destination + chat id.
//
// Routing table:
//
//	flow_alert / accumulation / cluster / ownership /
//	news_intel_actionable / outcome_followup
//	    → signal (Enabled iff SignalEnabled && SignalChatID != "")
//	signal_quality_report  → admin (Enabled iff per-surface toggle on)
//	watchtower_stats       → admin (gated by AdminStats)
//	strategy_scorecard     → admin (gated by AdminStrategyScorecard)
//	operational_health     → admin (gated by AdminOperationalHealth)
//	budget_report          → admin (gated by AdminBudgetReports)
//	suppression_report     → admin (gated by AdminSuppressionReports)
//	generic_market_intel / daily_political_intel / no_edge /
//	prediction_update / prediction_state_transition /
//	prediction_blocked
//	    → blocked (always; legacy)
//	any other (empty / unknown surface)
//	    → blocked with reason="unknown_surface"
//
// Disabled destinations resolve to DestinationSuppressed with a
// SuppressionReason; the caller logs + metric-bumps but does NOT
// retarget to the other chat.
func (c RouterConfig) Route(s Surface) RouteDecision {
	dec := RouteDecision{Surface: s}

	if IsBlockedSurface(s) {
		dec.Destination = DestinationBlocked
		dec.SuppressionReason = "legacy_surface"
		return dec
	}

	if IsSignalSurface(s) {
		dec.Destination = DestinationSignal
		if !c.SignalEnabled {
			dec.Destination = DestinationSuppressed
			dec.SuppressionReason = "signal_disabled"
			return dec
		}
		if strings.TrimSpace(c.SignalChatID) == "" {
			dec.Destination = DestinationSuppressed
			dec.SuppressionReason = "signal_chat_missing"
			return dec
		}
		dec.ChatID = c.SignalChatID
		dec.Enabled = true
		return dec
	}

	if IsAdminSurface(s) {
		dec.Destination = DestinationAdmin
		if !c.AdminEnabled {
			dec.Destination = DestinationSuppressed
			dec.SuppressionReason = "admin_disabled"
			return dec
		}
		if strings.TrimSpace(c.AdminChatID) == "" {
			dec.Destination = DestinationSuppressed
			dec.SuppressionReason = "admin_chat_missing"
			return dec
		}
		if !c.adminSurfaceEnabled(s) {
			dec.Destination = DestinationSuppressed
			dec.SuppressionReason = "admin_surface_disabled"
			return dec
		}
		dec.ChatID = c.AdminChatID
		dec.Enabled = true
		return dec
	}

	dec.Destination = DestinationBlocked
	dec.SuppressionReason = "unknown_surface"
	return dec
}

func (c RouterConfig) adminSurfaceEnabled(s Surface) bool {
	switch s {
	case SurfaceSignalQualityReport:
		return c.AdminSignalQualityReports
	case SurfaceWatchtowerStats:
		return c.AdminStats
	case SurfaceStrategyScorecard:
		return c.AdminStrategyScorecard
	case SurfaceOperationalHealth:
		return c.AdminOperationalHealth
	case SurfaceBudgetReport:
		return c.AdminBudgetReports
	case SurfaceSuppressionReport:
		return c.AdminSuppressionReports
	case SurfaceMarketCloseReview:
		// v11.4: gated by the same operator toggle as the
		// signal-quality report. The two reports share the
		// admin audience; carve out a separate toggle later
		// if the operator needs finer control.
		return c.AdminSignalQualityReports
	}
	return false
}

// --- Typed message + Sender ----------------------------------------------

// Message is the typed payload every caller hands the Router.
// Surface MUST be set; HTML is the rendered body. DisablePreview
// defaults to true at the transport boundary (we already disable
// link previews on every send) — the field is retained for future
// surfaces that may want previews enabled.
type Message struct {
	Surface        Surface
	HTML           string
	DisablePreview bool
	Metadata       map[string]string
}

// Sender is the typed external interface every worker uses.
// *Router satisfies it in production; tests pass a fake.
type Sender interface {
	Send(ctx context.Context, msg Message) (SendResult, error)
}

// HTMLSender (defined in guard.go) is the low-level transport
// interface. *Bot and *Guard both satisfy it. The outcome workers
// continue to use it directly because EditMessageText /
// SetMessageReaction live on *Bot and target an already-sent
// message id rather than dispatching a fresh send.

// --- Router (production Sender) ------------------------------------------

// RouterMetrics is the metric seam. Production wiring passes
// *metrics.Metrics through the metrics adapter; tests can supply
// a fake. Nil tolerated for cheap unit tests.
type RouterMetrics interface {
	ObserveRoute(surface, destination, decision string)
	ObserveSent(surface, destination, status string)
	ObserveSuppressed(surface, destination, reason string)
	ObserveSendFailed(surface, destination, reason string)
}

// nilMetrics is the zero-value RouterMetrics. Methods are no-ops.
type nilMetrics struct{}

func (nilMetrics) ObserveRoute(string, string, string)      {}
func (nilMetrics) ObserveSent(string, string, string)       {}
func (nilMetrics) ObserveSuppressed(string, string, string) {}
func (nilMetrics) ObserveSendFailed(string, string, string) {}

// Router is the production typed Sender. It owns the routing
// matrix + the underlying HTML transport. Concurrency-safe (no
// mutable state).
type Router struct {
	cfg     RouterConfig
	inner   HTMLSender
	metrics RouterMetrics
}

// NewRouter builds the Router. The inner transport is typically
// the guard-wrapped *Bot (so a legacy body that slipped through
// without a typed surface is still caught at the marker layer).
// Validate() is the caller's responsibility — fail loud at boot
// rather than at runtime.
func NewRouter(cfg RouterConfig, inner HTMLSender, met RouterMetrics) *Router {
	if met == nil {
		met = nilMetrics{}
	}
	return &Router{cfg: cfg, inner: inner, metrics: met}
}

// Send applies the routing decision and dispatches via the inner
// transport when the decision is reachable. Suppression / block
// outcomes return (zero SendResult, nil error) — they are not
// transport errors, they are operator-configured outcomes.
func (r *Router) Send(ctx context.Context, msg Message) (SendResult, error) {
	dec := r.cfg.Route(msg.Surface)

	r.metrics.ObserveRoute(string(msg.Surface), string(dec.Destination), routeDecisionLabel(dec))

	switch dec.Destination {
	case DestinationBlocked:
		r.metrics.ObserveSuppressed(string(msg.Surface), string(dec.Destination), dec.SuppressionReason)
		return SendResult{}, nil
	case DestinationSuppressed:
		r.metrics.ObserveSuppressed(string(msg.Surface), string(dec.Destination), dec.SuppressionReason)
		return SendResult{}, nil
	}

	if !dec.Enabled || dec.ChatID == "" {
		// Defensive — Route() already covers these cases, but a
		// future caller that builds RouteDecision by hand should
		// still be caught.
		r.metrics.ObserveSuppressed(string(msg.Surface), string(dec.Destination), "decision_unreachable")
		return SendResult{}, nil
	}

	res, err := r.inner.SendHTML(ctx, dec.ChatID, msg.HTML)
	if err != nil {
		r.metrics.ObserveSendFailed(string(msg.Surface), string(dec.Destination), classifySendErr(err))
		return res, err
	}
	r.metrics.ObserveSent(string(msg.Surface), string(dec.Destination), "ok")
	return res, nil
}

func routeDecisionLabel(d RouteDecision) string {
	switch d.Destination {
	case DestinationSignal, DestinationAdmin:
		return "allowed"
	case DestinationBlocked:
		return "blocked"
	case DestinationSuppressed:
		return "suppressed"
	}
	return "unknown"
}

// classifySendErr maps a transport error to a compact metric label
// (no PII / no raw provider text). Keeps the suppressed/failed
// counter cardinality bounded.
func classifySendErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "Client.Timeout"):
		return "timeout"
	case strings.Contains(msg, "chat not found"), strings.Contains(msg, "bot was kicked"):
		return "chat_unavailable"
	case strings.Contains(msg, "message is too long"):
		return "payload_too_long"
	case strings.Contains(msg, "parse"):
		return "parse_rejected"
	}
	return "other"
}

// --- Prometheus adapter (production metrics) -----------------------------

// PromMetricsAdapter projects a *prometheus.CounterVec set onto the
// RouterMetrics interface. Constructed in the metrics package and
// passed in at wiring time so this package doesn't import the
// metrics package (avoids an import cycle through repository).
type PromMetricsAdapter struct {
	Route      *prometheus.CounterVec
	Sent       *prometheus.CounterVec
	Suppressed *prometheus.CounterVec
	SendFailed *prometheus.CounterVec
}

func (a PromMetricsAdapter) ObserveRoute(surface, destination, decision string) {
	if a.Route != nil {
		a.Route.WithLabelValues(surface, destination, decision).Inc()
	}
}

func (a PromMetricsAdapter) ObserveSent(surface, destination, status string) {
	if a.Sent != nil {
		a.Sent.WithLabelValues(surface, destination, status).Inc()
	}
}

func (a PromMetricsAdapter) ObserveSuppressed(surface, destination, reason string) {
	if a.Suppressed != nil {
		a.Suppressed.WithLabelValues(surface, destination, reason).Inc()
	}
}

func (a PromMetricsAdapter) ObserveSendFailed(surface, destination, reason string) {
	if a.SendFailed != nil {
		a.SendFailed.WithLabelValues(surface, destination, reason).Inc()
	}
}
