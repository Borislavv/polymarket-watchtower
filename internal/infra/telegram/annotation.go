// annotation.go — typed adapter for non-Send Telegram API methods
// (EditMessageText / SetMessageReaction).
//
// The v11.3 Router exposes the typed Send path for new messages
// (chat id resolved by Surface). EditMessageText + SetMessageReaction
// don't fit that contract — they target an already-sent message id
// and the chat id comes from the row carrying the alert. This
// adapter wraps the same low-level *Bot but exposes typed methods
// that include the Surface label so metrics and logs stay
// attributable.
//
// Rules:
//   - The adapter NEVER sends a fresh message. New messages always
//     go through Router.Send.
//   - The adapter records per-(surface, action, status) metrics so
//     dashboards can distinguish edit vs reaction failures.
//   - The adapter does not gate on chat-id routing — the caller is
//     expected to pass the chat id the original alert was sent to
//     (typically the signal chat). Sending to the wrong chat would
//     succeed in the API but is operator error, not a routing
//     concern.
package telegram

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// AnnotationTransport is the typed surface for the
// non-Send Telegram operations the outcome-AI and the Market Close
// Review workers need.
//
// All methods accept a Surface so metric / log attribution stays
// consistent with the rest of the v11.3 typed routing surface.
type AnnotationTransport interface {
	EditOutcomeMessage(ctx context.Context, surface Surface, chatID string, messageID int64, html string) error
	SetOutcomeReaction(ctx context.Context, surface Surface, chatID string, messageID int64, emoji string) error
}

// EditReactionClient is the subset of *Bot the adapter wraps. Keeps
// the adapter unit-testable without spinning up a real bot.
type EditReactionClient interface {
	EditMessageText(ctx context.Context, chatID string, messageID int64, text string) error
	SetMessageReaction(ctx context.Context, chatID string, messageID int64, emoji string) error
}

// AnnotationMetrics is the metric seam. *PromAnnotationMetricsAdapter
// satisfies it in production; tests can pass a recording fake.
type AnnotationMetrics interface {
	ObserveAnnotation(surface, action, status string)
	ObserveAnnotationFailed(surface, action, reason string)
}

// nilAnnotationMetrics is a no-op fallback so the adapter can be
// constructed without metrics in cheap unit tests.
type nilAnnotationMetrics struct{}

func (nilAnnotationMetrics) ObserveAnnotation(string, string, string)       {}
func (nilAnnotationMetrics) ObserveAnnotationFailed(string, string, string) {}

// Annotation is the production AnnotationTransport. Holds the
// low-level *Bot client + the metric adapter.
type Annotation struct {
	client EditReactionClient
	met    AnnotationMetrics
}

// NewAnnotation wires a v11.4 annotation adapter. inner is typically
// the production *Bot (which satisfies EditReactionClient via its
// EditMessageText + SetMessageReaction methods).
func NewAnnotation(inner EditReactionClient, met AnnotationMetrics) *Annotation {
	if met == nil {
		met = nilAnnotationMetrics{}
	}
	return &Annotation{client: inner, met: met}
}

// EditOutcomeMessage replaces the body of a previously-sent message.
// Wraps Bot.EditMessageText with metric attribution.
//
// Errors are propagated unchanged — callers map ErrEditUnsupported
// onto a permanent-failure outcome (the original message is too old
// for Telegram to edit) and ordinary errors onto a retry.
func (a *Annotation) EditOutcomeMessage(ctx context.Context, surface Surface, chatID string, messageID int64, html string) error {
	err := a.client.EditMessageText(ctx, chatID, messageID, html)
	if err != nil {
		a.met.ObserveAnnotationFailed(string(surface), "edit", classifyEditErr(err))
		return err
	}
	a.met.ObserveAnnotation(string(surface), "edit", "ok")
	return nil
}

// SetOutcomeReaction posts a single emoji reaction onto an existing
// message. Wraps Bot.SetMessageReaction with metric attribution.
func (a *Annotation) SetOutcomeReaction(ctx context.Context, surface Surface, chatID string, messageID int64, emoji string) error {
	err := a.client.SetMessageReaction(ctx, chatID, messageID, emoji)
	if err != nil {
		a.met.ObserveAnnotationFailed(string(surface), "reaction", classifyReactionErr(err))
		return err
	}
	a.met.ObserveAnnotation(string(surface), "reaction", "ok")
	return nil
}

// classifyEditErr maps an edit-time error onto a compact metric
// label. ErrEditUnsupported is the canonical "message too old or
// bot lacks rights" case the outcome worker treats as permanent.
func classifyEditErr(err error) string {
	if errors.Is(err, ErrEditUnsupported) {
		return "unsupported"
	}
	return classifySendErr(err)
}

// classifyReactionErr is the reaction-specific equivalent of
// classifyEditErr. ErrReactionUnsupported is the canonical "this
// chat/message doesn't allow that emoji" case.
func classifyReactionErr(err error) string {
	if errors.Is(err, ErrReactionUnsupported) {
		return "unsupported"
	}
	return classifySendErr(err)
}

// PromAnnotationMetricsAdapter projects *prometheus.CounterVec set
// onto the AnnotationMetrics interface. Constructed in the metrics
// package and passed in at wiring time.
type PromAnnotationMetricsAdapter struct {
	Annotation       *prometheus.CounterVec
	AnnotationFailed *prometheus.CounterVec
}

func (a PromAnnotationMetricsAdapter) ObserveAnnotation(surface, action, status string) {
	if a.Annotation != nil {
		a.Annotation.WithLabelValues(surface, action, status).Inc()
	}
}

func (a PromAnnotationMetricsAdapter) ObserveAnnotationFailed(surface, action, reason string) {
	if a.AnnotationFailed != nil {
		a.AnnotationFailed.WithLabelValues(surface, action, reason).Inc()
	}
}
