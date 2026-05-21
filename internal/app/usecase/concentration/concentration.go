// Package concentration is the v10.8 per-event + per-wallet alert
// concentration / escalation gate.
//
// Audit motivation (4-day prod snapshot):
//
//   - 17 of 57 alerts (30%) came from a single event
//     (us-x-iran-permanent-peace-deal-by).
//   - 1 wallet generated 4 same-event accumulation alerts inside a
//     17-minute window.
//   - 21 of 57 alerts (37%) would have required escalation under a
//     "max 3 alerts per event per 24h, subsequent must clear 2×
//     previous notional" rule.
//
// Rules (deterministic, no AI):
//
//  1. Per-wallet, per-event: an alert is the FIRST when no prior
//     alert exists in the last WALLET_ALERT_COOLDOWN window. After
//     the first, subsequent alerts in the same window must clear
//     `prev_notional * ACCUMULATION_ESCALATION_FACTOR` AND advance
//     by at least one severity tier — otherwise they're SUPPRESSED.
//
//  2. Per-event: an event is allowed N=EVENT_ALERT_CONCENTRATION_LIMIT
//     non-suppressed alerts inside EVENT_ALERT_CONCENTRATION_WINDOW.
//     The (N+1)th alert and beyond require `prev_event_max_notional
//     * REPEATED_EVENT_THRESHOLD_MULTIPLIER` to clear.
//
// The gate ONLY answers "should this alert ship?" — it does not
// mutate the detector pipeline. The detect.Loop calls the gate after
// composing a Finding and BEFORE persisting the alert row.
//
// The package is pure (no I/O); the call site provides the recent-
// alerts window via the AlertHistory interface.
package concentration

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Config tunes the gate.
type Config struct {
	// EventConcentrationLimit: how many alerts an event may produce
	// in EventConcentrationWindow before the escalation factor
	// kicks in. 0 disables (legacy behaviour).
	EventConcentrationLimit int
	// EventConcentrationWindow: rolling window used by the limit.
	EventConcentrationWindow time.Duration
	// RepeatedEventThresholdMultiplier: the (N+1)th alert and beyond
	// must clear `prev_event_max_notional * this_factor` to pass.
	RepeatedEventThresholdMultiplier float64

	// WalletAlertCooldown: per-(wallet, event) cooldown. While
	// active, subsequent alerts apply the escalation factor below.
	WalletAlertCooldown time.Duration
	// AccumulationEscalationFactor: subsequent same-wallet alerts
	// in the cooldown must clear `prev_notional * factor`.
	AccumulationEscalationFactor float64

	// SeverityFloor is the severity at which the gate starts caring
	// about concentration. "info" is the typical setting — Warning
	// and above are rare enough to never trip the gate in practice.
	SeverityFloor string
}

func (c *Config) applyDefaults() {
	if c.EventConcentrationLimit <= 0 {
		c.EventConcentrationLimit = 3
	}
	if c.EventConcentrationWindow <= 0 {
		c.EventConcentrationWindow = 24 * time.Hour
	}
	if c.RepeatedEventThresholdMultiplier <= 1 {
		c.RepeatedEventThresholdMultiplier = 2
	}
	if c.WalletAlertCooldown <= 0 {
		c.WalletAlertCooldown = 6 * time.Hour
	}
	if c.AccumulationEscalationFactor <= 1 {
		c.AccumulationEscalationFactor = 2
	}
	if c.SeverityFloor == "" {
		c.SeverityFloor = "info"
	}
}

// Alert is the projection the gate needs from the persisted alert
// history. The caller (typically detect.Loop) fills this from
// polymarket_alerts.
type Alert struct {
	CreatedAt   time.Time
	EventSlug   string
	Wallet      string
	NotionalUSD float64
	Severity    string
}

// AlertHistory is the seam to the repository. Production wires it to
// `*repository.AlertRepository`; tests pass a slice.
type AlertHistory interface {
	// RecentAlertsForEvent returns alerts for the event in the given
	// window, newest-first ordering preferred but the gate sorts
	// internally.
	RecentAlertsForEvent(ctx context.Context, eventSlug string, since time.Time) ([]Alert, error)
	// RecentAlertsForWallet returns alerts the wallet generated on
	// the event in the given window.
	RecentAlertsForWallet(ctx context.Context, wallet, eventSlug string, since time.Time) ([]Alert, error)
}

// Candidate is what the gate evaluates: a finding the detector has
// composed but not yet persisted.
type Candidate struct {
	EventSlug   string
	Wallet      string
	NotionalUSD float64
	Severity    string
	Now         time.Time
}

// Decision is the gate result. Allow=true means the surface MAY
// persist the alert; Allow=false means it MUST NOT (the gate logs +
// metrics the reason).
type Decision struct {
	Allow               bool
	Reason              string
	RequiredNotional    float64 // the threshold the candidate failed to clear
	PriorAlertsInWindow int
	MaxPriorNotional    float64
}

// Gate is the deterministic gate. nil-safe construction is fine —
// no allocation lives on the struct.
type Gate struct {
	cfg Config
}

// New returns a Gate. zero Config gets the spec defaults.
func New(cfg Config) *Gate {
	cfg.applyDefaults()
	return &Gate{cfg: cfg}
}

// Evaluate runs both rules in order. The first failure short-circuits;
// the call returns the failing reason and the threshold the candidate
// fell short of.
//
// The gate ALWAYS allows alerts at or above the SeverityFloor's
// successor — "warning" and "critical" alerts bypass concentration
// checks because they are intrinsically rarer. The audit data shows
// 5/57 alerts (8.8%) were warning+, well under any operator cap.
func (g *Gate) Evaluate(ctx context.Context, in Candidate, history AlertHistory) (Decision, error) {
	if g == nil {
		return Decision{Allow: true, Reason: "gate_disabled"}, nil
	}
	if !severityAtOrAbove(in.Severity, g.cfg.SeverityFloor) {
		return Decision{Allow: true, Reason: "below_severity_floor"}, nil
	}
	// Warning and above bypass — they're rare enough to not need
	// concentration suppression and the operator wants every one.
	if severityAtOrAbove(in.Severity, "warning") {
		return Decision{Allow: true, Reason: "above_concentration_gate"}, nil
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}

	// Rule 1: per-wallet, per-event escalation.
	if history != nil && in.Wallet != "" && in.EventSlug != "" {
		walletSince := in.Now.Add(-g.cfg.WalletAlertCooldown)
		prior, err := history.RecentAlertsForWallet(ctx, in.Wallet, in.EventSlug, walletSince)
		if err == nil && len(prior) > 0 {
			maxPrev := 0.0
			for _, a := range prior {
				if a.NotionalUSD > maxPrev {
					maxPrev = a.NotionalUSD
				}
			}
			required := maxPrev * g.cfg.AccumulationEscalationFactor
			if in.NotionalUSD < required {
				return Decision{
					Allow:               false,
					Reason:              "wallet_escalation_failed",
					RequiredNotional:    required,
					PriorAlertsInWindow: len(prior),
					MaxPriorNotional:    maxPrev,
				}, nil
			}
		}
	}

	// Rule 2: per-event concentration cap.
	if history != nil && in.EventSlug != "" && g.cfg.EventConcentrationLimit > 0 {
		eventSince := in.Now.Add(-g.cfg.EventConcentrationWindow)
		prior, err := history.RecentAlertsForEvent(ctx, in.EventSlug, eventSince)
		if err == nil && len(prior) >= g.cfg.EventConcentrationLimit {
			maxPrev := 0.0
			for _, a := range prior {
				if a.NotionalUSD > maxPrev {
					maxPrev = a.NotionalUSD
				}
			}
			required := maxPrev * g.cfg.RepeatedEventThresholdMultiplier
			if in.NotionalUSD < required {
				return Decision{
					Allow:               false,
					Reason:              "event_concentration_cap",
					RequiredNotional:    required,
					PriorAlertsInWindow: len(prior),
					MaxPriorNotional:    maxPrev,
				}, nil
			}
		}
	}
	return Decision{Allow: true, Reason: "passed"}, nil
}

// severityAtOrAbove compares severity strings using the standard
// rank ordering (info < warning < critical < hard).
func severityAtOrAbove(s, floor string) bool {
	return severityRank(s) >= severityRank(floor)
}

func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return 1
	case "warning":
		return 2
	case "critical":
		return 3
	case "hard":
		return 4
	}
	return 0
}

// String surfaces the gate state for log lines.
func (d Decision) String() string {
	if d.Allow {
		return fmt.Sprintf("allow:%s", d.Reason)
	}
	return fmt.Sprintf("suppress:%s required=$%.0f priors=%d max_prev=$%.0f",
		d.Reason, d.RequiredNotional, d.PriorAlertsInWindow, d.MaxPriorNotional)
}
