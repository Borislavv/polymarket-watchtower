// shadow_decisions_repository.go — v11.5 audit + value-tracking
// repository wrapper. Implements
// shadowdecisions.Writer over the sqlc-generated
// polymarket_strategy_shadow_decisions queries.
package repository

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// ShadowDecisionsRepository writes per-detector audit rows.
type ShadowDecisionsRepository struct {
	q *sqlc.Queries
}

func NewShadowDecisionsRepository(pool *pgxpool.Pool) *ShadowDecisionsRepository {
	return &ShadowDecisionsRepository{q: sqlc.New(pool)}
}

// Record persists one detector decision. Reasons + Features are
// JSON-marshalled at the boundary; nil maps to NULL JSONB.
//
// CohortID + LinkedAlertDedupKey collapse to NULL when empty so
// the partial indexes on the table behave as intended.
func (r *ShadowDecisionsRepository) Record(ctx context.Context, d shadowdecisions.Decision) (int64, error) {
	reasonsJSON, err := shadowdecisions.MarshalReasons(d.Reasons)
	if err != nil {
		return 0, err
	}
	featuresJSON, err := shadowdecisions.MarshalFeatures(d.Features)
	if err != nil {
		return 0, err
	}

	var cohortID *string
	if d.CohortID != "" {
		v := d.CohortID
		cohortID = &v
	}
	var linkedDedup *string
	if d.LinkedAlertDedupKey != "" {
		v := d.LinkedAlertDedupKey
		linkedDedup = &v
	}

	id, err := r.q.InsertStrategyShadowDecision(ctx, sqlc.InsertStrategyShadowDecisionParams{
		StrategyName:        d.StrategyName,
		StrategyVersion:     d.StrategyVersion,
		ConditionID:         d.ConditionID,
		EventSlug:           d.EventSlug,
		Wallet:              d.Wallet,
		CohortID:            cohortID,
		Side:                d.Side,
		DecisionKind:        string(d.Kind),
		DecisionLevel:       string(d.Level),
		Score:               d.Score,
		Confidence:          d.Confidence,
		ReasonsJson:         reasonsJSON,
		FeaturesJson:        featuresJSON,
		ShadowOnly:          d.ShadowOnly,
		LinkedAlertDedupKey: linkedDedup,
		ControlBucketKey:    d.ControlBucketKey,
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// MarshalShadowJSON is a small helper so callers can build payloads
// without importing encoding/json. Used by integration tests.
func MarshalShadowJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
