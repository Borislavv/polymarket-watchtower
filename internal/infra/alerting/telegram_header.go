// telegram_header.go — v10.5 universal Telegram metadata header.
//
// Every Telegram-bound surface (alert / prediction / market intel /
// daily intel / outcome / system) emits a compact header BEFORE the
// surface-specific body. The header carries:
//
//	Type      — operator-visible classification.
//	Trigger   — "frequency=2h, last_posted_at=…, now=…" for regular
//	            reports; "by=<reason>" for triggered alerts.
//	Strategy  — human-readable strategy names (NOT raw enum dumps).
//	Value     — tokens / usd / profit when relevant.
//	AI        — cost / tokens / status (ok | skipped | error | fallback).
//
// Empty fields are omitted. Renders pure-HTML for Telegram parse_mode
// with html.EscapeString applied at every operator-visible boundary.
//
// The header is intentionally cheap to construct: callers fill the
// HeaderInput struct from data they already have, the renderer
// concatenates strings. No I/O.
package alerting

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// MessageType enumerates the operator-visible classification a
// Telegram message carries in its header. Values are stable; tests
// and dashboards key off them.
type MessageType string

const (
	MessageTypeRegular          MessageType = "regular"
	MessageTypeTriggered        MessageType = "triggered"
	MessageTypePrediction       MessageType = "prediction"
	MessageTypePredictionUpdate MessageType = "prediction_update"
	MessageTypeMarketIntel      MessageType = "market_intel"
	MessageTypeDailyIntel       MessageType = "daily_intel"
	MessageTypeOutcome          MessageType = "outcome"
	MessageTypeSystem           MessageType = "system"
)

// AIStatus is the small enum we expose in the header so an operator
// can tell at a glance whether the AI fired, was skipped, failed, or
// shipped a deterministic fallback.
type AIStatus string

const (
	AIStatusOK       AIStatus = "ok"
	AIStatusSkipped  AIStatus = "skipped"
	AIStatusError    AIStatus = "error"
	AIStatusFallback AIStatus = "fallback"
	AIStatusUnknown  AIStatus = "unknown"
)

// AIInfo carries the AI-call cost + token + status fields shown in
// the header. All zero/empty fields are elided.
type AIInfo struct {
	Status       AIStatus
	CostUSD      float64
	PromptTokens int
	OutputTokens int
}

// HeaderInput is the operator-facing shape. Fields are intentionally
// scalar — callers fill what they have. Empty values render nothing
// (no `Type: ` line on its own).
type HeaderInput struct {
	Type MessageType

	// Regular-report trigger fields.
	Frequency    time.Duration // e.g. 2h, 24h
	LastPostedAt time.Time
	Now          time.Time

	// Triggered-alert trigger fields.
	TriggeredBy string // operator-readable reason

	// Human-readable strategy names — pass through StrategyLabel().
	Strategies []string

	// Value fields. USD non-zero ⇒ printed.
	Tokens      string // free-text (e.g. "Yes · 100 shares")
	NotionalUSD float64
	ProfitUSD   float64

	// AI fields.
	AI AIInfo
}

// RenderHeader returns the rendered Telegram HTML header (terminated
// with a single blank line) plus a `present` boolean for callers that
// need to skip the header when no fields are populated.
//
// The header NEVER puts AI-authored text in titles — by construction
// it carries only operator-set fields.
func RenderHeader(in HeaderInput) string {
	var b strings.Builder
	if in.Type != "" {
		fmt.Fprintf(&b, "<b>Type:</b> %s\n", html.EscapeString(string(in.Type)))
	}
	if line := renderTriggerLine(in); line != "" {
		fmt.Fprintf(&b, "<b>Trigger:</b> %s\n", line)
	}
	if line := renderStrategyLine(in.Strategies); line != "" {
		fmt.Fprintf(&b, "<b>Strategy:</b> %s\n", line)
	}
	if line := renderValueLine(in); line != "" {
		fmt.Fprintf(&b, "<b>Value:</b> %s\n", line)
	}
	if line := renderAILine(in.AI); line != "" {
		fmt.Fprintf(&b, "<b>AI:</b> %s\n", line)
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteString("\n")
	return b.String()
}

func renderTriggerLine(in HeaderInput) string {
	// "by=<reason>" overrides regular-report fields when present —
	// triggered alerts carry their fire reason, not a cadence.
	if in.TriggeredBy != "" {
		return "by=" + html.EscapeString(in.TriggeredBy)
	}
	parts := []string{}
	if in.Frequency > 0 {
		parts = append(parts, "frequency="+formatDuration(in.Frequency))
	}
	if !in.LastPostedAt.IsZero() {
		parts = append(parts, "last_posted_at="+in.LastPostedAt.UTC().Format(time.RFC3339))
	}
	if !in.Now.IsZero() {
		parts = append(parts, "now="+in.Now.UTC().Format(time.RFC3339))
	}
	return html.EscapeString(strings.Join(parts, ", "))
}

func renderStrategyLine(strategies []string) string {
	out := make([]string, 0, len(strategies))
	seen := map[string]bool{}
	for _, s := range strategies {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// Always pass through StrategyLabel — raw enum keys are
		// allowed at the input boundary but operator-visible output
		// MUST be human-readable.
		label := StrategyLabel(s)
		if seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	if len(out) == 0 {
		return ""
	}
	for i, o := range out {
		out[i] = html.EscapeString(o)
	}
	return strings.Join(out, " · ")
}

func renderValueLine(in HeaderInput) string {
	parts := []string{}
	if in.Tokens != "" {
		parts = append(parts, "tokens="+html.EscapeString(in.Tokens))
	}
	if in.NotionalUSD > 0 {
		parts = append(parts, fmt.Sprintf("usd=$%.0f", in.NotionalUSD))
	}
	if in.ProfitUSD != 0 {
		// Signed — profit can be negative.
		parts = append(parts, fmt.Sprintf("profit=$%.0f", in.ProfitUSD))
	}
	return strings.Join(parts, ", ")
}

func renderAILine(ai AIInfo) string {
	parts := []string{}
	if ai.Status != "" {
		parts = append(parts, "status="+html.EscapeString(string(ai.Status)))
	}
	// Cost is always rendered when AI was invoked at all. status=ok
	// without cost is still legible ($0.0000), but operators
	// generally expect a cost number — print 0 explicitly.
	if ai.Status == AIStatusOK || ai.Status == AIStatusFallback || ai.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("cost=$%.4f", ai.CostUSD))
	} else if ai.Status == AIStatusSkipped {
		parts = append(parts, "cost=$0")
	}
	if ai.PromptTokens > 0 || ai.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens=%d/%d", ai.PromptTokens, ai.OutputTokens))
	}
	return strings.Join(parts, ", ")
}

// formatDuration prints durations in the operator's preferred shape
// (1h, 2h, 24h, 5m, …) rather than the verbose 2h0m0s form.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
