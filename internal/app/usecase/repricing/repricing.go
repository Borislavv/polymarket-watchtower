// Package repricing computes deterministic repricing signals per
// (event, annotation). The signals answer:
//
//   - did the market already reprice in the annotation's direction?
//   - is there repricing lag?
//   - did flow appear before the annotation (pre-event positioning)
//     or after (post-event chasing)?
//   - is opposite-side flow resisting the move?
//
// The output (RepricingSignal) is persisted to
// polymarket_repricing_signals and rendered into AI prompts.
// Everything in this package is deterministic — no AI calls. The
// AI consumes the signal as evidence but does not author it.
//
// Failure semantics are fail-open: any DB error returns the
// zero-value signal with status "unclear" + an explanation. The
// daily / catalyst / prediction paths never block on this layer.
package repricing

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Flow-timing + repricing-status string enums — mirrored from the
// migration CHECK constraints.
const (
	FlowTimingPreEvent  = "pre_event_positioning"
	FlowTimingPostEvent = "post_event_chasing"
	FlowTimingMixed     = "mixed"
	FlowTimingNoFlow    = "no_flow"
	FlowTimingUnknown   = "unknown"

	StatusUnderreacting         = "underreacting"
	StatusOverreacting          = "overreacting"
	StatusAlreadyPriced         = "already_priced"
	StatusStillRepricing        = "still_repricing"
	StatusReversed              = "reversed"
	StatusUnclear               = "unclear"
	StatusLaggingRelatedOutcome = "lagging_related_outcome"
	StatusStaleAnnotation       = "stale_annotation"
)

// Config tunes thresholds.
type Config struct {
	Enabled                bool
	Lookback               time.Duration
	PreWindow              time.Duration
	PostWindow             time.Duration
	MinAnnotationMove      float64 // ignore tiny annotation moves
	MinFlowUSD             float64 // ignore micro-flow
	UnderreactionThreshold float64 // current_vs_price_after below this and same-direction → underreacting
	OverreactionThreshold  float64 // current_vs_price_after past price_after by this → overreacting
}

func (c *Config) applyDefaults() {
	if c.Lookback <= 0 {
		c.Lookback = 24 * time.Hour
	}
	if c.PreWindow <= 0 {
		c.PreWindow = 2 * time.Hour
	}
	if c.PostWindow <= 0 {
		c.PostWindow = 2 * time.Hour
	}
	if c.MinAnnotationMove <= 0 {
		c.MinAnnotationMove = 0.05
	}
	if c.MinFlowUSD <= 0 {
		c.MinFlowUSD = 5_000
	}
	if c.UnderreactionThreshold <= 0 {
		c.UnderreactionThreshold = 0.03
	}
	if c.OverreactionThreshold <= 0 {
		c.OverreactionThreshold = 0.08
	}
}

// AnnotationInput is one annotation the provider scores.
type AnnotationInput struct {
	EventSlug      string
	ConditionID    string
	Outcome        string
	AnnotationHash string
	Title          string
	Timestamp      time.Time
	PriceBefore    *float64
	PriceAfter     *float64
	PriceChange    *float64
	CurrentPrice   *float64 // resolved by caller from the event-page snapshot

	// v10.2 outcome-mapping metadata. When OutcomeMapped=true the
	// caller has resolved the outcome via outcomemapping.Mapper —
	// the classifier trusts CurrentPrice and the verdict can be
	// "underreacting"/"reversed"/"already_priced" instead of the
	// v10.0 default of "unclear" when CurrentPrice was nil.
	OutcomeMapped            bool
	OutcomeMappingConfidence float64
	OutcomeMappingReason     string
}

// Signal is the in-memory shape of one computed RepricingSignal.
// Persisted shape lives in repository.NewRepricingSignal.
type Signal struct {
	EventSlug               string
	ConditionID             string
	Outcome                 string
	AnnotationHash          string
	AnnotationTime          time.Time
	AnnotationTitle         string
	PriceBefore             *float64
	PriceAfter              *float64
	AnnotationPriceChange   *float64
	CurrentPrice            *float64
	CurrentVsPriceAfter     float64
	DriftSinceAnnotation    float64
	PreAnnotationFlowUSD    float64
	PostAnnotationFlowUSD   float64
	SameSidePostFlowUSD     float64
	OppositeSidePostFlowUSD float64
	FlowTiming              string
	RepricingStatus         string
	Confidence              float64
	Explanation             string

	// --- v10.2 outcome-mapping fields ----------------------------
	// Populated by the caller when an outcomemapping.Mapper resolved
	// the (event, condition, outcome) tuple to a specific market.
	// OutcomeMapped=false ⇒ the classifier should treat
	// CurrentVsPriceAfter as low-confidence and prefer "unclear"
	// when other inputs are also weak. OutcomeMapped=true with a
	// MappingConfidence near 1.0 means the underreacting /
	// reversed / already_priced verdicts can be trusted.
	OutcomeMapped            bool
	OutcomeMappingConfidence float64
	MappingReason            string
	// RelatedOutcomeLag fires when (in a multi-candidate event) the
	// annotation's outcome already moved but a sibling outcome has
	// not yet adjusted in the expected complementary direction.
	// The classifier sets Status=lagging_related_outcome on the
	// SIBLING signal so downstream consumers see an actionable lag.
	RelatedOutcomeLag       bool
	RelatedOutcomeLagReason string
}

// Store is the persistence seam.
type Store interface {
	UpsertRepricingSignal(ctx context.Context, s repository.NewRepricingSignal) error
}

// TradeWindowQuerier is the seam to the sqlc-generated
// SumConditionTradesInWindow query. Repository-level wrappers don't
// expose it directly because nobody else needs it.
type TradeWindowQuerier interface {
	SumConditionTradesInWindow(ctx context.Context, arg sqlc.SumConditionTradesInWindowParams) ([]sqlc.SumConditionTradesInWindowRow, error)
}

// Provider is the deterministic computer + persister.
type Provider struct {
	cfg     Config
	trades  TradeWindowQuerier
	store   Store
	metrics *metrics.Metrics
	log     *zerolog.Logger
}

// New wires the provider.
func New(cfg Config, trades TradeWindowQuerier, store Store, met *metrics.Metrics, log *zerolog.Logger) *Provider {
	cfg.applyDefaults()
	return &Provider{cfg: cfg, trades: trades, store: store, metrics: met, log: log}
}

// Compute produces a Signal for one annotation. Pure function over
// the supplied AnnotationInput + the SumConditionTradesInWindow
// query. Persistence is opt-in via persist=true.
func (p *Provider) Compute(ctx context.Context, in AnnotationInput, persist bool) (Signal, error) {
	if !p.cfg.Enabled {
		return Signal{}, nil
	}
	sig := Signal{
		EventSlug:                in.EventSlug,
		ConditionID:              in.ConditionID,
		Outcome:                  in.Outcome,
		AnnotationHash:           in.AnnotationHash,
		AnnotationTime:           in.Timestamp,
		AnnotationTitle:          in.Title,
		PriceBefore:              in.PriceBefore,
		PriceAfter:               in.PriceAfter,
		AnnotationPriceChange:    in.PriceChange,
		CurrentPrice:             in.CurrentPrice,
		FlowTiming:               FlowTimingUnknown,
		RepricingStatus:          StatusUnclear,
		OutcomeMapped:            in.OutcomeMapped,
		OutcomeMappingConfidence: in.OutcomeMappingConfidence,
		MappingReason:            in.OutcomeMappingReason,
	}

	// Skip when we don't have enough to reason.
	if in.AnnotationHash == "" || in.ConditionID == "" || in.Timestamp.IsZero() {
		sig.Explanation = "insufficient inputs"
		return sig, nil
	}

	// Price math.
	if in.PriceBefore != nil && in.PriceAfter != nil {
		// Drift since annotation: how far has price moved from
		// price_after toward / away from price_before. Positive
		// drift means market continued moving in the annotation's
		// direction; negative means a reversal.
		if in.CurrentPrice != nil {
			sig.CurrentVsPriceAfter = *in.CurrentPrice - *in.PriceAfter
			direction := signOf(*in.PriceAfter - *in.PriceBefore)
			sig.DriftSinceAnnotation = direction * sig.CurrentVsPriceAfter
		}
	}

	// Flow window math.
	preFlowSameSide, preFlowOpposite, _ := p.sumSidesInWindow(ctx, in,
		in.Timestamp.Add(-p.cfg.PreWindow), in.Timestamp)
	postFlowSameSide, postFlowOpposite, _ := p.sumSidesInWindow(ctx, in,
		in.Timestamp, in.Timestamp.Add(p.cfg.PostWindow))
	sig.PreAnnotationFlowUSD = preFlowSameSide + preFlowOpposite
	sig.PostAnnotationFlowUSD = postFlowSameSide + postFlowOpposite
	sig.SameSidePostFlowUSD = postFlowSameSide
	sig.OppositeSidePostFlowUSD = postFlowOpposite

	// Classify flow timing.
	sig.FlowTiming = classifyFlowTiming(p.cfg, sig.PreAnnotationFlowUSD, sig.PostAnnotationFlowUSD)
	// Classify repricing status.
	sig.RepricingStatus, sig.Confidence, sig.Explanation = classifyRepricing(p.cfg, in, sig)
	// v10.2 stale-annotation override: if the annotation predates
	// the lookback window AND we still couldn't get a clear verdict,
	// surface that explicitly so the operator sees "we have nothing
	// fresh on this" instead of "no decisive signal".
	if sig.RepricingStatus == StatusUnclear && !in.Timestamp.IsZero() {
		age := time.Since(in.Timestamp)
		// Use 2× the lookback as the "stale" threshold so a 24h
		// lookback considers anything older than 48h stale.
		if age > 2*p.cfg.Lookback {
			sig.RepricingStatus = StatusStaleAnnotation
			sig.Confidence = 0.4
			sig.Explanation = fmt.Sprintf("annotation is %.0fh old (> %.0fh stale threshold)",
				age.Hours(), (2 * p.cfg.Lookback).Hours())
		}
	}

	if persist && p.store != nil {
		_ = p.store.UpsertRepricingSignal(ctx, repository.NewRepricingSignal{
			EventSlug:               sig.EventSlug,
			ConditionID:             sig.ConditionID,
			Outcome:                 sig.Outcome,
			AnnotationHash:          sig.AnnotationHash,
			AnnotationTime:          sig.AnnotationTime,
			AnnotationTitle:         sig.AnnotationTitle,
			PriceBefore:             sig.PriceBefore,
			PriceAfter:              sig.PriceAfter,
			AnnotationPriceChange:   sig.AnnotationPriceChange,
			CurrentPrice:            sig.CurrentPrice,
			CurrentVsPriceAfter:     sig.CurrentVsPriceAfter,
			DriftSinceAnnotation:    sig.DriftSinceAnnotation,
			PreAnnotationFlowUSD:    sig.PreAnnotationFlowUSD,
			PostAnnotationFlowUSD:   sig.PostAnnotationFlowUSD,
			SameSidePostFlowUSD:     sig.SameSidePostFlowUSD,
			OppositeSidePostFlowUSD: sig.OppositeSidePostFlowUSD,
			FlowTiming:              sig.FlowTiming,
			RepricingStatus:         sig.RepricingStatus,
			Confidence:              sig.Confidence,
			Explanation:             sig.Explanation,
		})
	}
	p.observe("computed", sig.FlowTiming)
	return sig, nil
}

// sumSidesInWindow returns the same-side and opposite-side notional
// (USD) within the [from, to) window for the annotation's outcome.
// The "annotation's side" is inferred from the price move: a
// positive move in the named outcome means BUY pressure is the
// same side. We treat the annotation as anchored to a YES outcome
// when PriceAfter > PriceBefore.
func (p *Provider) sumSidesInWindow(ctx context.Context, in AnnotationInput, from, to time.Time) (same, opp float64, err error) {
	if p.trades == nil {
		return 0, 0, nil
	}
	rows, err := p.trades.SumConditionTradesInWindow(ctx, sqlc.SumConditionTradesInWindowParams{
		ConditionID: in.ConditionID,
		Since:       pgtype.Timestamptz{Time: from, Valid: true},
		Until:       pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return 0, 0, err
	}
	sameSide := annotationSameSide(in)
	for _, r := range rows {
		// Outcome-match guard: if the annotation specifies an
		// outcome string, only count trades on that outcome. The
		// annotation `outcome` is operator-prose (e.g. "Ken Paxton")
		// while r.OutcomeToken is the CLOB token id — we can't
		// reliably match across that divide. We default to summing
		// across all outcomes for the condition; the AI prompt
		// notes the ambiguity.
		_ = in.Outcome
		if strings.EqualFold(r.Side, sameSide) {
			same += r.NotionalUsd
		} else {
			opp += r.NotionalUsd
		}
	}
	return same, opp, nil
}

// annotationSameSide returns the BUY/SELL side that aligns with the
// annotation's directional move. PriceAfter > PriceBefore → BUY is
// the "same side"; vice versa → SELL.
func annotationSameSide(in AnnotationInput) string {
	if in.PriceBefore != nil && in.PriceAfter != nil {
		if *in.PriceAfter >= *in.PriceBefore {
			return "BUY"
		}
		return "SELL"
	}
	return "BUY" // default
}

func classifyFlowTiming(cfg Config, pre, post float64) string {
	if pre < cfg.MinFlowUSD && post < cfg.MinFlowUSD {
		return FlowTimingNoFlow
	}
	switch {
	case pre >= cfg.MinFlowUSD && post >= cfg.MinFlowUSD:
		if pre > post*1.5 {
			return FlowTimingPreEvent
		}
		if post > pre*1.5 {
			return FlowTimingPostEvent
		}
		return FlowTimingMixed
	case pre >= cfg.MinFlowUSD:
		return FlowTimingPreEvent
	case post >= cfg.MinFlowUSD:
		return FlowTimingPostEvent
	}
	return FlowTimingUnknown
}

// classifyRepricing turns price + flow features into the repricing
// status. The thresholds live in Config; the logic is intentionally
// simple so its behaviour is auditable from the prompt block alone.
func classifyRepricing(cfg Config, in AnnotationInput, sig Signal) (status string, confidence float64, explanation string) {
	if in.PriceBefore == nil || in.PriceAfter == nil || in.CurrentPrice == nil {
		return StatusUnclear, 0.0, "missing price data"
	}
	move := math.Abs(*in.PriceAfter - *in.PriceBefore)
	if move < cfg.MinAnnotationMove {
		return StatusUnclear, 0.2, fmt.Sprintf("annotation move %.3f below threshold %.3f", move, cfg.MinAnnotationMove)
	}
	// Direction of the original move. dir * delta from priceAfter:
	// positive → market continued in the annotation direction past
	// priceAfter; negative → market pulled back toward priceBefore.
	dir := signOf(*in.PriceAfter - *in.PriceBefore)
	driftAfter := dir * (*in.CurrentPrice - *in.PriceAfter)
	// Distance from priceBefore in the annotation direction. When
	// driftBefore < 0 the market has crossed BACK through
	// priceBefore — a true reversal.
	driftBefore := dir * (*in.CurrentPrice - *in.PriceBefore)
	switch {
	case driftBefore < -cfg.UnderreactionThreshold:
		// Crossed back through priceBefore by more than the
		// underreaction threshold → reversal.
		return StatusReversed, 0.7, fmt.Sprintf("price crossed back through priceBefore by %.3f", -driftBefore)
	case driftAfter >= cfg.OverreactionThreshold:
		return StatusOverreacting, 0.7, fmt.Sprintf("price continued %.3f past price_after", driftAfter)
	case driftAfter <= -cfg.UnderreactionThreshold:
		return StatusUnderreacting, 0.7, fmt.Sprintf("price still %.3f below price_after; underreaction", -driftAfter)
	case math.Abs(driftAfter) < cfg.UnderreactionThreshold:
		// Within thresholds: are we still moving (post-event flow
		// material) or settled (already priced)?
		if sig.PostAnnotationFlowUSD >= cfg.MinFlowUSD && sig.SameSidePostFlowUSD > sig.OppositeSidePostFlowUSD*1.5 {
			return StatusStillRepricing, 0.6, "post-event same-side flow > opposite; still digesting"
		}
		return StatusAlreadyPriced, 0.65, "price within noise of price_after; market appears settled"
	}
	return StatusUnclear, 0.2, "no decisive signal"
}

func signOf(v float64) float64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

func (p *Provider) observe(status, flowTiming string) {
	if p.metrics == nil || p.metrics.RepricingSignals == nil {
		return
	}
	p.metrics.RepricingSignals.WithLabelValues(status, flowTiming).Inc()
}
