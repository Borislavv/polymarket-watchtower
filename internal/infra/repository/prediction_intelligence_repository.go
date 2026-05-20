// prediction_intelligence_repository.go — v10.2 usefulness +
// feedback persistence.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// PredictionIntelligenceRepository wraps the sqlc-generated helpers
// for v10.2's two new tables. Separate type from
// RepricingPredictionsRepository so the dependency graph stays
// readable: workers that don't care about usefulness/feedback don't
// pull in this repo at all.
type PredictionIntelligenceRepository struct {
	q *sqlc.Queries
}

func NewPredictionIntelligenceRepository(pool *pgxpool.Pool) *PredictionIntelligenceRepository {
	return &PredictionIntelligenceRepository{q: sqlc.New(pool)}
}

// UsefulnessScoreInput is the upsert shape. components_json is
// pre-marshaled by the scorer.
type UsefulnessScoreInput struct {
	PredictionID   int64
	Score          float64
	ComponentsJSON []byte
	Reason         string
}

// UpsertUsefulnessScore writes the live score for one prediction.
func (r *PredictionIntelligenceRepository) UpsertUsefulnessScore(ctx context.Context, in UsefulnessScoreInput) (int64, error) {
	return r.q.UpsertPredictionUsefulnessScore(ctx, sqlc.UpsertPredictionUsefulnessScoreParams{
		PredictionID:   in.PredictionID,
		Score:          in.Score,
		ComponentsJson: in.ComponentsJSON,
		Reason:         in.Reason,
	})
}

// FeedbackRow mirrors polymarket_prediction_feedback fields used by
// the worker. NULL pointers map to NULL columns; the sqlc generated
// shape uses pgtype variants under the hood.
type FeedbackRow struct {
	PredictionID             int64
	Horizon                  string
	PriceAtPrediction        *float64
	PriceAtHorizon           *float64
	PriceDelta               *float64
	DirectionCorrect         *bool
	StateAtHorizon           string
	RepricingStatusAtHorizon string
	CatalystStatusAtHorizon  string
	FlowConfirmed            bool
}

// UpsertFeedback writes one (prediction_id, horizon) measurement.
// Optional text fields ("" → NULL) and optional pointer fields
// (nil → NULL) flow through verbatim — sqlc-generated params use
// pgtype-friendly *T types directly.
func (r *PredictionIntelligenceRepository) UpsertFeedback(ctx context.Context, in FeedbackRow) error {
	var stateAt, repAt, catAt *string
	if in.StateAtHorizon != "" {
		s := in.StateAtHorizon
		stateAt = &s
	}
	if in.RepricingStatusAtHorizon != "" {
		s := in.RepricingStatusAtHorizon
		repAt = &s
	}
	if in.CatalystStatusAtHorizon != "" {
		s := in.CatalystStatusAtHorizon
		catAt = &s
	}
	return r.q.UpsertPredictionFeedback(ctx, sqlc.UpsertPredictionFeedbackParams{
		PredictionID:             in.PredictionID,
		Horizon:                  in.Horizon,
		PriceAtPrediction:        in.PriceAtPrediction,
		PriceAtHorizon:           in.PriceAtHorizon,
		PriceDelta:               in.PriceDelta,
		DirectionCorrect:         in.DirectionCorrect,
		StateAtHorizon:           stateAt,
		RepricingStatusAtHorizon: repAt,
		CatalystStatusAtHorizon:  catAt,
		FlowConfirmed:            in.FlowConfirmed,
	})
}

// FeedbackCandidate is one row returned from ListPredictionsForFeedback.
type FeedbackCandidate struct {
	ID           int64
	EventSlug    string
	ConditionID  string
	Outcome      string
	SideBias     string
	Confidence   float64
	CurrentState string
	CreatedAt    time.Time
}

// ListPredictionsForFeedback returns predictions older than the
// smallest horizon that are not yet resolved/invalidated.
func (r *PredictionIntelligenceRepository) ListPredictionsForFeedback(ctx context.Context, oldest time.Time, limit int32) ([]FeedbackCandidate, error) {
	rows, err := r.q.ListPredictionsForFeedback(ctx, sqlc.ListPredictionsForFeedbackParams{
		OldestEligible: tsFromTime(oldest),
		LimitCount:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list predictions for feedback: %w", err)
	}
	out := make([]FeedbackCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, FeedbackCandidate{
			ID:           r.ID,
			EventSlug:    r.EventSlug,
			ConditionID:  r.ConditionID,
			Outcome:      r.Outcome,
			SideBias:     r.SideBias,
			Confidence:   r.Confidence,
			CurrentState: r.CurrentState,
			CreatedAt:    r.CreatedAt.Time,
		})
	}
	return out, nil
}

// ArchivalCandidate is one row returned by ListPredictionsForArchival.
type ArchivalCandidate struct {
	ID           int64
	EventSlug    string
	ConditionID  string
	CurrentState string
	UpdatedAt    time.Time
}

// ListPredictionsForArchival returns terminal predictions whose
// updated_at < olderThan (i.e. aged past terminal retention).
func (r *PredictionIntelligenceRepository) ListPredictionsForArchival(ctx context.Context, olderThan time.Time, limit int32) ([]ArchivalCandidate, error) {
	rows, err := r.q.ListPredictionsForArchival(ctx, sqlc.ListPredictionsForArchivalParams{
		OlderThan:  tsFromTime(olderThan),
		LimitCount: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ArchivalCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, ArchivalCandidate{
			ID:           r.ID,
			EventSlug:    r.EventSlug,
			ConditionID:  r.ConditionID,
			CurrentState: r.CurrentState,
			UpdatedAt:    r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// ArchivePrediction stamps archived_at + the operator-facing reason.
func (r *PredictionIntelligenceRepository) ArchivePrediction(ctx context.Context, id int64, terminalReason string) error {
	return r.q.ArchivePrediction(ctx, sqlc.ArchivePredictionParams{
		ID:             id,
		TerminalReason: &terminalReason,
	})
}

// ListPredictionsForStaleSignal returns active predictions older
// than the stale-no-signal threshold for the worker's secondary
// sweep.
func (r *PredictionIntelligenceRepository) ListPredictionsForStaleSignal(ctx context.Context, olderThan time.Time, limit int32) ([]ArchivalCandidate, error) {
	rows, err := r.q.ListPredictionsForStaleSignal(ctx, sqlc.ListPredictionsForStaleSignalParams{
		OlderThan:  tsFromTime(olderThan),
		LimitCount: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ArchivalCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, ArchivalCandidate{
			ID:           r.ID,
			EventSlug:    r.EventSlug,
			ConditionID:  r.ConditionID,
			CurrentState: r.CurrentState,
			UpdatedAt:    r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// MarkPredictionStaleNoSignal stamps state=stale + the reason on
// one prediction. Idempotent at the SQL level.
func (r *PredictionIntelligenceRepository) MarkPredictionStaleNoSignal(ctx context.Context, id int64, reason string) error {
	return r.q.MarkPredictionStaleNoSignal(ctx, sqlc.MarkPredictionStaleNoSignalParams{
		ID:     id,
		Reason: reason,
	})
}

// PredictionEvaluationRow is the upsert shape for one evaluation
// row written by the v10.3 prediction-evaluation pipeline.
type PredictionEvaluationRow struct {
	PredictionID int64
	Horizon      string
	Evaluation   string
	Score        float64
	EvidenceJSON []byte
}

// UpsertEvaluation writes one classifier output per
// (prediction_id, horizon).
func (r *PredictionIntelligenceRepository) UpsertEvaluation(ctx context.Context, in PredictionEvaluationRow) error {
	return r.q.UpsertPredictionEvaluation(ctx, sqlc.UpsertPredictionEvaluationParams{
		PredictionID: in.PredictionID,
		Horizon:      in.Horizon,
		Evaluation:   in.Evaluation,
		Score:        in.Score,
		EvidenceJson: in.EvidenceJSON,
	})
}

// HorizonsRecorded returns the set of horizons already measured for
// one prediction. The worker subtracts this from the configured
// horizons list to find the missing work.
func (r *PredictionIntelligenceRepository) HorizonsRecorded(ctx context.Context, predictionID int64) (map[string]bool, error) {
	rows, err := r.q.ListHorizonsRecorded(ctx, predictionID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, h := range rows {
		out[h] = true
	}
	return out, nil
}
