package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// --- Repricing signals --------------------------------------------------

// RepricingSignal is the domain projection of one
// polymarket_repricing_signals row.
type RepricingSignal struct {
	ID                      int64
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
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// NewRepricingSignal is the upsert input. Keyed on
// (event_slug, condition_id, annotation_hash).
type NewRepricingSignal struct {
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
}

// --- Market predictions -------------------------------------------------

// MarketPrediction is the domain projection of one
// polymarket_market_predictions row.
type MarketPrediction struct {
	ID                        int64
	EventSlug                 string
	ConditionID               string
	Outcome                   string
	SideBias                  string
	Summary                   string
	CurrentState              string
	StateReason               string
	PreviousPredictionID      int64
	SupersedesPredictionID    int64
	LastRepricedAt            time.Time
	LastConfirmedByAlertAt    time.Time
	LastContradictedByAlertAt time.Time
	Confidence                float64
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// NewMarketPrediction is the upsert input. Keyed on
// (event_slug, condition_id).
type NewMarketPrediction struct {
	EventSlug                 string
	ConditionID               string
	Outcome                   string
	SideBias                  string
	Summary                   string
	CurrentState              string
	StateReason               string
	Confidence                float64
	LastRepricedAt            time.Time
	LastConfirmedByAlertAt    time.Time
	LastContradictedByAlertAt time.Time
}

// MarketPredictionStateTransition is one append-only audit row.
type MarketPredictionStateTransition struct {
	ID            int64
	PredictionID  int64
	PreviousState string
	NewState      string
	Reason        string
	EvidenceJSON  []byte
	CreatedAt     time.Time
}

// NewMarketPredictionStateTransition is the insert input.
type NewMarketPredictionStateTransition struct {
	PredictionID  int64
	PreviousState string
	NewState      string
	Reason        string
	EvidenceJSON  []byte
}

// ErrPredictionNotFound is returned when no prediction row exists.
var ErrPredictionNotFound = errors.New("market prediction not found")

// RepricingPredictionsRepository owns the v9.8 intelligence-hardening
// tables.
type RepricingPredictionsRepository struct {
	q *sqlc.Queries
}

func NewRepricingPredictionsRepository(pool *pgxpool.Pool) *RepricingPredictionsRepository {
	return &RepricingPredictionsRepository{q: sqlc.New(pool)}
}

// --- Repricing -----------------------------------------------------------

func (r *RepricingPredictionsRepository) UpsertRepricingSignal(ctx context.Context, s NewRepricingSignal) error {
	return r.q.UpsertRepricingSignal(ctx, sqlc.UpsertRepricingSignalParams{
		EventSlug:               s.EventSlug,
		ConditionID:             s.ConditionID,
		Outcome:                 s.Outcome,
		AnnotationHash:          s.AnnotationHash,
		AnnotationTime:          tsFromTime(s.AnnotationTime),
		AnnotationTitle:         s.AnnotationTitle,
		PriceBefore:             s.PriceBefore,
		PriceAfter:              s.PriceAfter,
		AnnotationPriceChange:   s.AnnotationPriceChange,
		CurrentPrice:            s.CurrentPrice,
		CurrentVsPriceAfter:     s.CurrentVsPriceAfter,
		DriftSinceAnnotation:    s.DriftSinceAnnotation,
		PreAnnotationFlowUsd:    s.PreAnnotationFlowUSD,
		PostAnnotationFlowUsd:   s.PostAnnotationFlowUSD,
		SameSidePostFlowUsd:     s.SameSidePostFlowUSD,
		OppositeSidePostFlowUsd: s.OppositeSidePostFlowUSD,
		FlowTiming:              s.FlowTiming,
		RepricingStatus:         s.RepricingStatus,
		Confidence:              s.Confidence,
		Explanation:             s.Explanation,
	})
}

func (r *RepricingPredictionsRepository) ListRepricingSignals(ctx context.Context, eventSlug string, limit int32) ([]RepricingSignal, error) {
	rows, err := r.q.ListRepricingSignalsForEvent(ctx, sqlc.ListRepricingSignalsForEventParams{
		EventSlug:  eventSlug,
		LimitCount: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list repricing signals: %w", err)
	}
	out := make([]RepricingSignal, 0, len(rows))
	for _, row := range rows {
		out = append(out, repricingFromSQLC(row))
	}
	return out, nil
}

// --- Predictions ---------------------------------------------------------

func (r *RepricingPredictionsRepository) GetPrediction(ctx context.Context, eventSlug, conditionID string) (MarketPrediction, error) {
	row, err := r.q.GetMarketPrediction(ctx, sqlc.GetMarketPredictionParams{
		EventSlug:   eventSlug,
		ConditionID: conditionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MarketPrediction{}, ErrPredictionNotFound
		}
		return MarketPrediction{}, fmt.Errorf("get market prediction: %w", err)
	}
	out := MarketPrediction{
		ID:                        row.ID,
		EventSlug:                 row.EventSlug,
		ConditionID:               row.ConditionID,
		Outcome:                   row.Outcome,
		SideBias:                  row.SideBias,
		Summary:                   row.Summary,
		CurrentState:              row.CurrentState,
		StateReason:               row.StateReason,
		LastRepricedAt:            tsTime(row.LastRepricedAt),
		LastConfirmedByAlertAt:    tsTime(row.LastConfirmedByAlertAt),
		LastContradictedByAlertAt: tsTime(row.LastContradictedByAlertAt),
		Confidence:                row.Confidence,
		CreatedAt:                 row.CreatedAt.Time,
		UpdatedAt:                 row.UpdatedAt.Time,
	}
	if row.PreviousPredictionID != nil {
		out.PreviousPredictionID = *row.PreviousPredictionID
	}
	if row.SupersedesPredictionID != nil {
		out.SupersedesPredictionID = *row.SupersedesPredictionID
	}
	return out, nil
}

// UpsertPrediction returns the persisted prediction id. Mutable
// fields refresh on conflict; the state machine on top of this
// repo decides which fields to bump.
func (r *RepricingPredictionsRepository) UpsertPrediction(ctx context.Context, p NewMarketPrediction) (int64, error) {
	return r.q.UpsertMarketPrediction(ctx, sqlc.UpsertMarketPredictionParams{
		EventSlug:                 p.EventSlug,
		ConditionID:               p.ConditionID,
		Outcome:                   p.Outcome,
		SideBias:                  p.SideBias,
		Summary:                   p.Summary,
		CurrentState:              p.CurrentState,
		StateReason:               p.StateReason,
		Confidence:                p.Confidence,
		LastRepricedAt:            tsFromTime(p.LastRepricedAt),
		LastConfirmedByAlertAt:    tsFromTime(p.LastConfirmedByAlertAt),
		LastContradictedByAlertAt: tsFromTime(p.LastContradictedByAlertAt),
	})
}

func (r *RepricingPredictionsRepository) RecordStateTransition(ctx context.Context, t NewMarketPredictionStateTransition) error {
	return r.q.InsertMarketPredictionStateTransition(ctx, sqlc.InsertMarketPredictionStateTransitionParams{
		PredictionID:  t.PredictionID,
		PreviousState: t.PreviousState,
		NewState:      t.NewState,
		Reason:        t.Reason,
		EvidenceJson:  t.EvidenceJSON,
	})
}

// CountPredictionsCreatedSince returns the number of predictions
// created (created_at >= since). Used by the prediction-creation
// worker to enforce its per-day cap.
func (r *RepricingPredictionsRepository) CountPredictionsCreatedSince(ctx context.Context, since time.Time) (int64, error) {
	return r.q.CountPredictionsCreatedSince(ctx, tsFromTime(since))
}

// CountPredictionsForEventSince supports the per-event dedupe window
// in the prediction-creation worker. Counts every row (any state) so
// a recent stale prediction still suppresses recreation.
func (r *RepricingPredictionsRepository) CountPredictionsForEventSince(ctx context.Context, eventSlug string, since time.Time) (int64, error) {
	return r.q.CountPredictionsForEventSince(ctx, sqlc.CountPredictionsForEventSinceParams{
		EventSlug: eventSlug,
		Since:     tsFromTime(since),
	})
}

// ListPredictionsForEvolution returns the selection queue for the
// evolution worker. maxAge is the latest last_evolved_at value a
// row may have — anything stricter than that is "fresh enough" and
// gets skipped this cycle.
func (r *RepricingPredictionsRepository) ListPredictionsForEvolution(ctx context.Context, maxAge time.Time, limit int32) ([]MarketPrediction, error) {
	rows, err := r.q.ListPredictionsForEvolution(ctx, sqlc.ListPredictionsForEvolutionParams{
		MaxAge:     tsFromTime(maxAge),
		LimitCount: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list predictions for evolution: %w", err)
	}
	out := make([]MarketPrediction, 0, len(rows))
	for _, row := range rows {
		out = append(out, MarketPrediction{
			ID:                        row.ID,
			EventSlug:                 row.EventSlug,
			ConditionID:               row.ConditionID,
			Outcome:                   row.Outcome,
			SideBias:                  row.SideBias,
			Summary:                   row.Summary,
			CurrentState:              row.CurrentState,
			StateReason:               row.StateReason,
			LastRepricedAt:            tsTime(row.LastRepricedAt),
			LastConfirmedByAlertAt:    tsTime(row.LastConfirmedByAlertAt),
			LastContradictedByAlertAt: tsTime(row.LastContradictedByAlertAt),
			Confidence:                row.Confidence,
			CreatedAt:                 row.CreatedAt.Time,
			UpdatedAt:                 row.UpdatedAt.Time,
		})
		if row.PreviousPredictionID != nil {
			out[len(out)-1].PreviousPredictionID = *row.PreviousPredictionID
		}
		if row.SupersedesPredictionID != nil {
			out[len(out)-1].SupersedesPredictionID = *row.SupersedesPredictionID
		}
	}
	return out, nil
}

// TouchPredictionEvolution drops the prediction to the back of the
// selection queue without bumping state/confidence/updated_at. The
// worker calls this on every processed row.
func (r *RepricingPredictionsRepository) TouchPredictionEvolution(ctx context.Context, id int64) error {
	return r.q.TouchPredictionEvolution(ctx, id)
}

// ApplyPredictionDecay decreases confidence by delta, clamped to
// floor. Bumps last_evolved_at + updated_at + state_reason.
func (r *RepricingPredictionsRepository) ApplyPredictionDecay(ctx context.Context, id int64, delta, floor float64, reason string) error {
	return r.q.ApplyPredictionDecay(ctx, sqlc.ApplyPredictionDecayParams{
		ID:     id,
		Delta:  delta,
		Floor:  floor,
		Reason: reason,
	})
}

func (r *RepricingPredictionsRepository) ListPredictionStates(ctx context.Context, predictionID int64, limit int32) ([]MarketPredictionStateTransition, error) {
	rows, err := r.q.ListMarketPredictionStates(ctx, sqlc.ListMarketPredictionStatesParams{
		PredictionID: predictionID,
		LimitCount:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list market prediction states: %w", err)
	}
	out := make([]MarketPredictionStateTransition, 0, len(rows))
	for _, row := range rows {
		out = append(out, MarketPredictionStateTransition{
			ID:            row.ID,
			PredictionID:  row.PredictionID,
			PreviousState: row.PreviousState,
			NewState:      row.NewState,
			Reason:        row.Reason,
			EvidenceJSON:  row.EvidenceJson,
			CreatedAt:     row.CreatedAt.Time,
		})
	}
	return out, nil
}

// --- conversions ---------------------------------------------------------

func repricingFromSQLC(row sqlc.PolymarketRepricingSignals) RepricingSignal {
	return RepricingSignal{
		ID:                      row.ID,
		EventSlug:               row.EventSlug,
		ConditionID:             row.ConditionID,
		Outcome:                 row.Outcome,
		AnnotationHash:          row.AnnotationHash,
		AnnotationTime:          tsTime(row.AnnotationTime),
		AnnotationTitle:         row.AnnotationTitle,
		PriceBefore:             row.PriceBefore,
		PriceAfter:              row.PriceAfter,
		AnnotationPriceChange:   row.AnnotationPriceChange,
		CurrentPrice:            row.CurrentPrice,
		CurrentVsPriceAfter:     row.CurrentVsPriceAfter,
		DriftSinceAnnotation:    row.DriftSinceAnnotation,
		PreAnnotationFlowUSD:    row.PreAnnotationFlowUsd,
		PostAnnotationFlowUSD:   row.PostAnnotationFlowUsd,
		SameSidePostFlowUSD:     row.SameSidePostFlowUsd,
		OppositeSidePostFlowUSD: row.OppositeSidePostFlowUsd,
		FlowTiming:              row.FlowTiming,
		RepricingStatus:         row.RepricingStatus,
		Confidence:              row.Confidence,
		Explanation:             row.Explanation,
		CreatedAt:               row.CreatedAt.Time,
		UpdatedAt:               row.UpdatedAt.Time,
	}
}

func predictionFromSQLC(row sqlc.PolymarketMarketPredictions) MarketPrediction {
	prev := int64(0)
	if row.PreviousPredictionID != nil {
		prev = *row.PreviousPredictionID
	}
	sup := int64(0)
	if row.SupersedesPredictionID != nil {
		sup = *row.SupersedesPredictionID
	}
	return MarketPrediction{
		ID:                        row.ID,
		EventSlug:                 row.EventSlug,
		ConditionID:               row.ConditionID,
		Outcome:                   row.Outcome,
		SideBias:                  row.SideBias,
		Summary:                   row.Summary,
		CurrentState:              row.CurrentState,
		StateReason:               row.StateReason,
		PreviousPredictionID:      prev,
		SupersedesPredictionID:    sup,
		LastRepricedAt:            tsTime(row.LastRepricedAt),
		LastConfirmedByAlertAt:    tsTime(row.LastConfirmedByAlertAt),
		LastContradictedByAlertAt: tsTime(row.LastContradictedByAlertAt),
		Confidence:                row.Confidence,
		CreatedAt:                 row.CreatedAt.Time,
		UpdatedAt:                 row.UpdatedAt.Time,
	}
}
