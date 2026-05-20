// Package marketprediction owns the deterministic state machine
// that drives polymarket_market_predictions transitions and the
// deterministic alert/prediction match scorer.
//
// Nothing here is AI-authored. The state machine is a small pure
// function of (current prediction, catalyst rows, repricing
// signal, flow summary, recent alerts) → next state + reason +
// JSON evidence blob.
//
// The package also exposes a Telegram renderer ("Prediction state"
// block) the News & Prediction Telegram formatter consumes.
package marketprediction

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Canonical state strings. Mirror the migration CHECK constraint.
const (
	StateNew                = "new"
	StateWatching           = "watching"
	StateBlocked            = "blocked"
	StateActiveCatalyst     = "active_catalyst"
	StateConfirmedByFlow    = "confirmed_by_flow"
	StateContradictedByFlow = "contradicted_by_flow"
	StateRepricing          = "repricing"
	StateAlreadyPriced      = "already_priced"
	StateStale              = "stale"
	StateResolved           = "resolved"
	StateInvalidated        = "invalidated"
)

// Config tunes the transition thresholds.
type Config struct {
	Enabled                 bool
	StaleAfter              time.Duration // updated_at older than this with no fresh evidence → stale
	ConfirmAlertScoreFloor  float64       // alerts at or above this match score confirm
	ContradictFlowImbalance float64       // |imbalance| ≥ this and direction == opposite → contradict
}

func (c *Config) applyDefaults() {
	if c.StaleAfter <= 0 {
		c.StaleAfter = 24 * time.Hour
	}
	if c.ConfirmAlertScoreFloor <= 0 {
		c.ConfirmAlertScoreFloor = 0.60
	}
	if c.ContradictFlowImbalance <= 0 {
		c.ContradictFlowImbalance = 0.65
	}
}

// Inputs is the state-machine's input bundle. Every field is
// optional — zero values produce a "watching" state with empty
// reason when nothing decisive is supplied.
type Inputs struct {
	Now                 time.Time
	Prediction          repository.MarketPrediction
	ActiveCatalysts     []repository.EventCatalyst // expected or active only
	RepricingSignal     *repricing.Signal          // optional
	FlowSummary         eventflow.EventFlowSummary
	MatchedAlerts       []MatchedAlert // already scored
	CatalystInvalidated bool           // operator/AI invalidated a catalyst this cycle
}

// Decision is the state machine's output.
type Decision struct {
	NewState      string
	PreviousState string
	Reason        string
	EvidenceJSON  []byte
	Changed       bool
}

// Decide is a pure function — given Inputs, produce the next state.
// Resolution / invalidation always win; otherwise the order below
// (blocked → confirmed → contradicted → repricing/already-priced →
// stale → watching) reflects priority for an active prediction.
func Decide(in Inputs, cfg Config) Decision {
	cfg.applyDefaults()
	prev := in.Prediction.CurrentState
	if prev == "" {
		prev = StateNew
	}
	dec := Decision{PreviousState: prev, NewState: prev}

	// Resolution / invalidation overrides.
	if isResolvedCatalyst(in.ActiveCatalysts) {
		dec.NewState = StateResolved
		dec.Reason = "active catalyst resolved"
		dec.EvidenceJSON = encodeEvidence(map[string]any{"catalysts": catalystEvidence(in.ActiveCatalysts)})
		dec.Changed = prev != dec.NewState
		return dec
	}
	if in.CatalystInvalidated {
		dec.NewState = StateInvalidated
		dec.Reason = "catalyst thesis invalidated"
		dec.EvidenceJSON = encodeEvidence(map[string]any{"catalyst_invalidated": true})
		dec.Changed = prev != dec.NewState
		return dec
	}

	// Blocked by active or expected catalyst — v10.2: only when the
	// catalyst's expected_at is still in the reasonable future. A
	// catalyst whose expected_at is far in the past is "stale-blocked"
	// and we let the prediction fall through to flow / repricing /
	// stale logic instead of pinning it to blocked forever.
	if blocking, allStale := classifyBlockingCatalysts(in.ActiveCatalysts, in.Now); blocking {
		dec.NewState = StateBlocked
		ev := catalystEvidence(in.ActiveCatalysts)
		dec.Reason = "active catalyst blocks repricing"
		dec.EvidenceJSON = encodeEvidence(map[string]any{"catalysts": ev})
		dec.Changed = prev != dec.NewState
		return dec
	} else if allStale && prev == StateBlocked {
		// We previously held blocked; the catalyst expected_at is
		// past + no confirmation arrived. Note this transition in
		// the reason so the dashboard shows the revalidation.
		dec.Reason = "blocked revalidated: catalyst expected_at passed without confirmation"
		dec.EvidenceJSON = encodeEvidence(map[string]any{"catalysts_stale": catalystEvidence(in.ActiveCatalysts)})
		// Fall through to the remaining classifiers (flow,
		// repricing, stale TTL, watching).
	}

	// Confirmed-by-flow: a matched alert clears the score floor AND
	// is directionally aligned.
	if confirming := bestAligned(in.MatchedAlerts, "aligned", cfg.ConfirmAlertScoreFloor); confirming != nil {
		dec.NewState = StateConfirmedByFlow
		dec.Reason = fmt.Sprintf("matched alert score=%.2f aligned with prediction", confirming.Score)
		dec.EvidenceJSON = encodeEvidence(map[string]any{"matched_alert": confirming})
		dec.Changed = prev != dec.NewState
		return dec
	}
	// Contradicted-by-flow: either a matched alert says "contradict"
	// at the score floor, OR the flow summary's opposite-side
	// imbalance is dominant.
	if contradicting := bestAligned(in.MatchedAlerts, "contradict", cfg.ConfirmAlertScoreFloor); contradicting != nil {
		dec.NewState = StateContradictedByFlow
		dec.Reason = fmt.Sprintf("matched alert score=%.2f contradicts prediction", contradicting.Score)
		dec.EvidenceJSON = encodeEvidence(map[string]any{"matched_alert": contradicting})
		dec.Changed = prev != dec.NewState
		return dec
	}
	if !in.FlowSummary.Empty() && in.FlowSummary.DirectionalImbalance < -cfg.ContradictFlowImbalance {
		dec.NewState = StateContradictedByFlow
		dec.Reason = fmt.Sprintf("opposite-side flow dominant (imbalance %+.2f)", in.FlowSummary.DirectionalImbalance)
		dec.EvidenceJSON = encodeEvidence(map[string]any{"flow_imbalance": in.FlowSummary.DirectionalImbalance})
		dec.Changed = prev != dec.NewState
		return dec
	}

	// Repricing / already-priced come from the deterministic signal.
	if in.RepricingSignal != nil {
		switch in.RepricingSignal.RepricingStatus {
		case repricing.StatusStillRepricing, repricing.StatusUnderreacting:
			dec.NewState = StateRepricing
			dec.Reason = "deterministic repricing signal: " + in.RepricingSignal.RepricingStatus
			dec.EvidenceJSON = encodeEvidence(map[string]any{"repricing": in.RepricingSignal})
			dec.Changed = prev != dec.NewState
			return dec
		case repricing.StatusAlreadyPriced:
			dec.NewState = StateAlreadyPriced
			dec.Reason = "deterministic repricing signal: already_priced"
			dec.EvidenceJSON = encodeEvidence(map[string]any{"repricing": in.RepricingSignal})
			dec.Changed = prev != dec.NewState
			return dec
		}
	}

	// Stale TTL: no fresh evidence in StaleAfter window.
	if !in.Prediction.UpdatedAt.IsZero() {
		lastFresh := mostRecent(
			in.Prediction.LastRepricedAt,
			in.Prediction.LastConfirmedByAlertAt,
			in.Prediction.LastContradictedByAlertAt,
		)
		if lastFresh.IsZero() {
			lastFresh = in.Prediction.UpdatedAt
		}
		if in.Now.Sub(lastFresh) > cfg.StaleAfter {
			dec.NewState = StateStale
			dec.Reason = fmt.Sprintf("no fresh evidence for %s", roundDur(in.Now.Sub(lastFresh)))
			dec.EvidenceJSON = encodeEvidence(map[string]any{"last_fresh_at": lastFresh.UTC()})
			dec.Changed = prev != dec.NewState
			return dec
		}
	}

	// Default: watching.
	dec.NewState = StateWatching
	if !in.FlowSummary.Empty() {
		dec.Reason = fmt.Sprintf("no decisive signal; %d alerts in window", in.FlowSummary.RecentAlerts)
	} else {
		dec.Reason = "no decisive signal; waiting for catalyst or material flow"
	}
	dec.Changed = prev != dec.NewState
	return dec
}

// Applier persists the decision: upserts the prediction row, then
// appends a transition audit row when state changed.
type Applier struct {
	repo    PredictionStore
	metrics *metrics.Metrics
	log     *zerolog.Logger
}

// PredictionStore is the seam to repository.RepricingPredictionsRepository.
type PredictionStore interface {
	UpsertPrediction(ctx context.Context, p repository.NewMarketPrediction) (int64, error)
	RecordStateTransition(ctx context.Context, t repository.NewMarketPredictionStateTransition) error
}

func NewApplier(repo PredictionStore, met *metrics.Metrics, log *zerolog.Logger) *Applier {
	return &Applier{repo: repo, metrics: met, log: log}
}

// Apply writes the new state. The supplied row is the input the
// caller wants persisted with the new state stamped on top.
func (a *Applier) Apply(ctx context.Context, in repository.NewMarketPrediction, dec Decision) error {
	in.CurrentState = dec.NewState
	in.StateReason = dec.Reason
	id, err := a.repo.UpsertPrediction(ctx, in)
	if err != nil {
		return fmt.Errorf("upsert prediction: %w", err)
	}
	if !dec.Changed {
		return nil
	}
	if err := a.repo.RecordStateTransition(ctx, repository.NewMarketPredictionStateTransition{
		PredictionID:  id,
		PreviousState: dec.PreviousState,
		NewState:      dec.NewState,
		Reason:        dec.Reason,
		EvidenceJSON:  dec.EvidenceJSON,
	}); err != nil {
		if a.log != nil {
			a.log.Warn().Err(err).Int64("prediction_id", id).Msg("marketprediction: transition record failed")
		}
	}
	if a.metrics != nil && a.metrics.MarketPredictionStateTransitions != nil {
		a.metrics.MarketPredictionStateTransitions.WithLabelValues(dec.PreviousState, dec.NewState).Inc()
	}
	return nil
}

// RenderTelegramBlock formats the Prediction state for the News &
// Prediction Telegram body. Renders HTML-escaped, omits when empty.
func RenderTelegramBlock(pred repository.MarketPrediction, dec Decision) string {
	if pred.CurrentState == "" && dec.NewState == "" {
		return ""
	}
	state := dec.NewState
	if state == "" {
		state = pred.CurrentState
	}
	var b strings.Builder
	b.WriteString("<b>Prediction state</b>\n")
	fmt.Fprintf(&b, "• state: %s\n", html.EscapeString(state))
	if r := strings.TrimSpace(dec.Reason); r != "" {
		fmt.Fprintf(&b, "• reason: %s\n", html.EscapeString(r))
	} else if r := strings.TrimSpace(pred.StateReason); r != "" {
		fmt.Fprintf(&b, "• reason: %s\n", html.EscapeString(r))
	}
	if dec.PreviousState != "" && dec.PreviousState != state {
		fmt.Fprintf(&b, "• changed from: %s\n", html.EscapeString(dec.PreviousState))
	}
	if len(dec.EvidenceJSON) > 0 {
		if compact := compactJSON(dec.EvidenceJSON); compact != "" {
			fmt.Fprintf(&b, "• evidence: %s\n", html.EscapeString(compact))
		}
	}
	return b.String()
}

// --- helpers -------------------------------------------------------------

func hasActiveCatalyst(rows []repository.EventCatalyst) bool {
	for _, c := range rows {
		if c.Status == repository.CatalystStatusActive || c.Status == repository.CatalystStatusExpected {
			return true
		}
	}
	return false
}

// classifyBlockingCatalysts decides whether the catalysts currently
// justify holding a prediction in the "blocked" state. v10.2 fix:
// a catalyst whose expected_at is already in the past for more than
// the staleGrace window is no longer blocking — the prediction
// should fall through to the deterministic flow / repricing / stale
// classifiers instead of being pinned to blocked forever.
//
// Returns (blocking, allStale):
//
//	blocking=true        → at least one active/expected catalyst with
//	                       expected_at in the future (or unknown).
//	allStale=true        → there ARE active/expected rows, but every
//	                       one of them has expected_at well in the
//	                       past — the operator-visible "revalidate"
//	                       branch in Decide flags this so the
//	                       reason carries the explanation.
//
// `staleGrace` is fixed at 24h: a runoff scheduled 48h ago that
// never had a confirmation catalyst means our information is stale,
// not that the market is still waiting.
func classifyBlockingCatalysts(rows []repository.EventCatalyst, now time.Time) (blocking, allStale bool) {
	const staleGrace = 24 * time.Hour
	any := false
	stillFuture := false
	allPast := true
	for _, c := range rows {
		if c.Status != repository.CatalystStatusActive && c.Status != repository.CatalystStatusExpected {
			continue
		}
		any = true
		if c.ExpectedAt.IsZero() {
			// Unknown expected_at — treat as still blocking (we
			// have no reason to call it stale).
			stillFuture = true
			allPast = false
			continue
		}
		if c.ExpectedAt.After(now.Add(-staleGrace)) {
			stillFuture = true
			allPast = false
		}
	}
	if !any {
		return false, false
	}
	return stillFuture, allPast
}

func isResolvedCatalyst(rows []repository.EventCatalyst) bool {
	for _, c := range rows {
		if c.Status == repository.CatalystStatusResolved {
			return true
		}
	}
	return false
}

func catalystEvidence(rows []repository.EventCatalyst) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"type":   string(c.CatalystType),
			"title":  c.Title,
			"status": string(c.Status),
		})
	}
	return out
}

func bestAligned(rows []MatchedAlert, alignment string, floor float64) *MatchedAlert {
	var best *MatchedAlert
	for i := range rows {
		r := rows[i]
		if r.Score < floor {
			continue
		}
		if r.DirectionAlignment != alignment {
			continue
		}
		if best == nil || r.Score > best.Score {
			best = &r
		}
	}
	return best
}

func mostRecent(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func roundDur(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

func encodeEvidence(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	if len(b) > 2048 {
		b = b[:2047]
	}
	return b
}

func compactJSON(b []byte) string {
	s := string(b)
	if len(s) > 220 {
		s = s[:219] + "…"
	}
	return s
}
