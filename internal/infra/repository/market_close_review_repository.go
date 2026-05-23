// market_close_review_repository.go — v11.4 Market Close Review
// persistence wrapper. Mirrors the other v11.x repository files:
// thin shim over sqlc, no domain logic.
package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// MarketCloseReviewCandidate is the projection used by the worker
// to pick the next market to review.
type MarketCloseReviewCandidate struct {
	MarketID    int64
	ConditionID string
	EventSlug   string
	Question    string
	EndDate     time.Time
	Closed      bool
	Active      bool
}

// MarketCloseReviewFinish is the v11.4 succeeded-payload the
// worker hands the repo after a clean AI run.
type MarketCloseReviewFinish struct {
	ID               int64
	Verdict          string
	Confidence       *float64
	AdminSummary     string
	AIJSON           []byte
	EvidenceJSON     []byte
	AIModel          string
	InputTokens      *int32
	OutputTokens     *int32
	EstimatedCostUSD *float64
}

// MarketCloseReviewRepository wraps polymarket_market_close_reviews.
type MarketCloseReviewRepository struct {
	q *sqlc.Queries
}

func NewMarketCloseReviewRepository(pool *pgxpool.Pool) *MarketCloseReviewRepository {
	return &MarketCloseReviewRepository{q: sqlc.New(pool)}
}

// ListCandidates returns recently-closed markets with no
// succeeded review row, ordered by closed_at DESC. Bounded by
// (closedSince, closedUntil, limit) so the scan stays cheap.
func (r *MarketCloseReviewRepository) ListCandidates(ctx context.Context, closedSince, closedUntil time.Time, limit int32) ([]MarketCloseReviewCandidate, error) {
	rows, err := r.q.ListMarketCloseReviewCandidates(ctx, sqlc.ListMarketCloseReviewCandidatesParams{
		ClosedSince: tsFromTime(closedSince),
		ClosedUntil: tsFromTime(closedUntil),
		RowLimit:    limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MarketCloseReviewCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, MarketCloseReviewCandidate{
			MarketID:    row.ID,
			ConditionID: row.ConditionID,
			EventSlug:   derefStr(row.EventSlug),
			Question:    row.Question,
			EndDate:     tsTime(row.EndDate),
			Closed:      row.Closed,
			Active:      row.Active,
		})
	}
	return out, nil
}

// HasSucceededReview reports whether a market already has a
// terminal succeeded review row. Bounded by the partial unique
// index defined in migration 00028.
func (r *MarketCloseReviewRepository) HasSucceededReview(ctx context.Context, conditionID string) (bool, error) {
	_, err := r.q.GetMarketCloseReview(ctx, conditionID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// InsertRunning opens a fresh review row in 'running' state and
// returns its id. The worker uses the id for the subsequent
// FinishSucceeded / FinishFailed / FinishSkipped call.
func (r *MarketCloseReviewRepository) InsertRunning(ctx context.Context, marketID int64, conditionID, eventSlug string, closedAt, resolvedAt time.Time) (int64, error) {
	var mid *int64
	if marketID > 0 {
		v := marketID
		mid = &v
	}
	return r.q.InsertMarketCloseReviewRunning(ctx, sqlc.InsertMarketCloseReviewRunningParams{
		MarketID:    mid,
		ConditionID: conditionID,
		EventSlug:   eventSlug,
		ClosedAt:    tsFromTime(closedAt),
		ResolvedAt:  tsFromTime(resolvedAt),
	})
}

// FinishSucceeded stamps the review row with the AI verdict,
// cost, and JSON payloads. Append-only contract: this is the
// only path that flips status -> 'succeeded'.
func (r *MarketCloseReviewRepository) FinishSucceeded(ctx context.Context, in MarketCloseReviewFinish) error {
	return r.q.FinishMarketCloseReviewSucceeded(ctx, sqlc.FinishMarketCloseReviewSucceededParams{
		ID:               in.ID,
		Verdict:          in.Verdict,
		Confidence:       in.Confidence,
		AdminSummary:     in.AdminSummary,
		AiJson:           in.AIJSON,
		EvidenceJson:     in.EvidenceJSON,
		AiModel:          in.AIModel,
		InputTokens:      in.InputTokens,
		OutputTokens:     in.OutputTokens,
		EstimatedCostUsd: in.EstimatedCostUSD,
	})
}

// FinishFailed bumps the attempts counter + schedules a retry.
func (r *MarketCloseReviewRepository) FinishFailed(ctx context.Context, id int64, errMsg string, nextRetryAt time.Time) error {
	return r.q.FinishMarketCloseReviewFailed(ctx, sqlc.FinishMarketCloseReviewFailedParams{
		ID:          id,
		Error:       errMsg,
		NextRetryAt: tsFromTime(nextRetryAt),
	})
}

// FinishSkipped records an operator-visible skip reason.
func (r *MarketCloseReviewRepository) FinishSkipped(ctx context.Context, id int64, reason string) error {
	return r.q.FinishMarketCloseReviewSkipped(ctx, sqlc.FinishMarketCloseReviewSkippedParams{
		ID:         id,
		SkipReason: reason,
	})
}

// ListAlertsForReview is the bounded alerts read the evidence
// pack uses. Returns alerts in ascending sent_at order so the
// worker can pick the most recent N within the prompt cap.
func (r *MarketCloseReviewRepository) ListAlertsForReview(ctx context.Context, marketID int64, sentSince time.Time, limit int32) ([]Alert, error) {
	rows, err := r.q.ListAlertsForMarketCloseReview(ctx, sqlc.ListAlertsForMarketCloseReviewParams{
		MarketID:  &marketID,
		SentSince: tsFromTime(sentSince),
		RowLimit:  limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(rows))
	for _, row := range rows {
		a := Alert{
			ID:                row.ID,
			DedupKey:          row.DedupKey,
			StrategyVersion:   row.StrategyVersion,
			Kind:              AlertKind(row.Kind),
			Reason:            row.Reason,
			Severity:          row.Severity,
			MarketID:          row.MarketID,
			TraderID:          row.TraderID,
			TradeID:           row.TradeID,
			Payload:           row.Payload,
			TelegramMessageID: row.TelegramMessageID,
			SentAt:            tsTime(row.SentAt),
			OutcomeStatus:     OutcomeStatus(row.OutcomeStatus),
			DriftStatus:       DriftStatus(row.DriftStatus),
			CLV15m:            row.Clv15m,
			CLV1h:             row.Clv1h,
			CLV6h:             row.Clv6h,
			CLV24h:            row.Clv24h,
			CreatedAt:         row.CreatedAt.Time,
		}
		out = append(out, a)
	}
	return out, nil
}

// jsonMessage wraps a JSON-marshalable any into raw bytes — used
// by the worker to build AIJSON / EvidenceJSON before handing the
// repo a finish call. nil maps to NULL JSONB.
func MarshalReviewJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// isNoRows is a small shim to centralize pgx.ErrNoRows checks
// without importing pgx in every repository file. The sqlc
// generated code returns pgx.ErrNoRows directly on :one queries
// when the row is missing.
func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	const noRows = "no rows in result set"
	return errString(err) == noRows || containsErrString(err, noRows)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func containsErrString(err error, needle string) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ts placeholder to silence unused import warnings during initial
// scaffolding when no Timestamptz helper is needed.
var _ pgtype.Timestamptz
