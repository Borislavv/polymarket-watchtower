// strategy_phase_b.go — v11.6 Phase B composition root.
//
// Builds:
//   - strategybus.Bus with the production PromotionGate;
//   - rulesrisk.Detector for the detect.Loop hook;
//   - strategyvalue.Worker that backfills CLV columns on shadow rows;
//   - strategypromotion.Worker that re-evaluates promotion criteria
//     every Interval and persists rows to
//     polymarket_strategy_promotion_reviews.
//
// All three are no-ops when Postgres is not wired or when the
// StrategyConfig.GlobalPromotionAllowed plus per-strategy flags
// keep the entire stack inert (the v11.5 / v11.6 default).
//
// Helpers exposed:
//   - wireStrategyPhaseB — called from New() inside the Postgres
//     branch, returns the Bus + the two new workers.
//   - registerStrategyPhaseBExecs — appends Exec entries to the
//     graceful-shutdown plan.
package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/strategybus"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/strategypromotion"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/strategyvalue"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// StrategyPhaseB is the bundle returned by wireStrategyPhaseB.
type StrategyPhaseB struct {
	Bus          *strategybus.Bus
	RulesRisk    *rulesrisk.Detector
	ValueWorker  *strategyvalue.Worker
	PromotionRev *strategypromotion.Worker
}

// wireStrategyPhaseB constructs the v11.6 bus + workers when Postgres
// is wired. Returns a zero-value bundle (all nil fields) when pool
// is nil — caller treats nil fields as "feature disabled".
func wireStrategyPhaseB(
	pool *pgxpool.Pool,
	scfg StrategyConfig,
	met *metrics.Metrics,
	log *zerolog.Logger,
) StrategyPhaseB {
	if pool == nil {
		return StrategyPhaseB{}
	}
	shadowRepo := repository.NewShadowDecisionsRepository(pool)
	// Promotion worker. The bus's PromotionGate is the worker itself.
	// v11.7: thresholds come from StrategyConfig instead of hardcoded.
	promo := strategypromotion.New(
		strategypromotion.Config{
			Enabled:              true,
			Interval:             scfg.PromotionReviewInterval,
			Lookback:             scfg.PromotionReviewLookback,
			MinSampleSize:        scfg.PromotionMinSample,
			MinSignedMove6hCents: scfg.PromotionMinSignedMove6hCents,
			MaxReversal15mRatio:  scfg.PromotionMaxReversal15mRatio,
			MaxAlertsPerDay:      scfg.PromotionMaxAlertsPerDay,
			// Force-disable kill-switch: either the canonical
			// STRATEGY_PROMOTION_FORCE_DISABLE or the legacy alias
			// STRATEGY_PROMOTION_BYPASS_EXPLICIT being true closes
			// the gate. Logical OR — never silently downgrade either.
			BypassExplicit: scfg.PromotionForceDisable || scfg.PromotionBypassExplicit,
		},
		newPromotionSampleAdapter(pool),
		newPromotionWriterAdapter(pool),
		met,
		nil,
	)

	// Build the bus with the production PromotionGate. log threading
	// is reserved for a future per-event audit line.
	_ = log
	bus := buildStrategyBusWithGate(scfg, shadowRepo, met, promo)

	rr := rulesrisk.New(rulesrisk.Config{})

	val := strategyvalue.New(
		strategyvalue.Config{
			Enabled:   true,
			Interval:  15 * time.Minute,
			BatchSize: 500,
			MaxAge:    30 * 24 * time.Hour,
		},
		newShadowPendingAdapter(pool),
		newShadowPriceAdapter(pool),
		newShadowUpdateAdapter(pool),
		met,
		nil,
	)
	return StrategyPhaseB{
		Bus:          bus,
		RulesRisk:    rr,
		ValueWorker:  val,
		PromotionRev: promo,
	}
}

// buildStrategyBusWithGate is a small variant of BuildStrategyBus
// that also wires the PromotionGate. Kept here so the legacy v11.5
// helper stays single-purpose.
func buildStrategyBusWithGate(s StrategyConfig, writer shadowdecisions.Writer, met *metrics.Metrics, gate strategybus.PromotionGate) *strategybus.Bus {
	flags := map[string]strategybus.StrategyFlag{
		"thesisaccum":     {Name: "thesisaccum", Enabled: s.ThesisAccum.Enabled, ShadowOnly: s.ThesisAccum.ShadowOnly},
		"holderdelta":     {Name: "holderdelta", Enabled: s.OwnershipV2.Enabled, ShadowOnly: s.OwnershipV2.ShadowOnly},
		"catalystwindow":  {Name: "catalystwindow", Enabled: s.CatalystWindow.Enabled, ShadowOnly: s.CatalystWindow.ShadowOnly},
		"bookvacuum":      {Name: "bookvacuum", Enabled: s.BookVacuum.Enabled, ShadowOnly: s.BookVacuum.ShadowOnly},
		"repricinglag":    {Name: "repricinglag", Enabled: s.RepricingLag.Enabled, ShadowOnly: s.RepricingLag.ShadowOnly},
		"walletcohort":    {Name: "walletcohort", Enabled: s.WalletCohort.Enabled, ShadowOnly: s.WalletCohort.ShadowOnly},
		"conflictresolve": {Name: "conflictresolve", Enabled: s.ConflictResolve.Enabled, ShadowOnly: s.ConflictResolve.ShadowOnly},
		"rulesrisk":       {Name: "rulesrisk", Enabled: s.RulesRisk.Enabled, ShadowOnly: s.RulesRisk.ShadowOnly},
		"cheaptail":       {Name: "cheaptail", Enabled: s.CheapTail.Enabled, ShadowOnly: s.CheapTail.ShadowOnly},
	}
	cfg := strategybus.Config{
		StrategyVersion:        s.StrategyVersion,
		GlobalPromotionAllowed: s.GlobalPromotionAllowed,
		Flags:                  flags,
		PromotionGate:          gate,
	}
	return strategybus.New(cfg, writer, met, nil)
}

// --- strategyvalue adapters --------------------------------------

type shadowPendingAdapter struct{ q *sqlc.Queries }

func newShadowPendingAdapter(pool *pgxpool.Pool) *shadowPendingAdapter {
	return &shadowPendingAdapter{q: sqlc.New(pool)}
}

func (a *shadowPendingAdapter) ListPendingValueRows(ctx context.Context, maxAge time.Duration, limit int) ([]strategyvalue.PendingRow, error) {
	cutoff := time.Now().Add(-maxAge)
	rows, err := a.q.ListPendingValueRows(ctx, sqlc.ListPendingValueRowsParams{
		MaxAge:   tsFromTime(cutoff),
		RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]strategyvalue.PendingRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, strategyvalue.PendingRow{
			ID:           r.ID,
			StrategyName: r.StrategyName,
			ConditionID:  r.ConditionID,
			Side:         r.Side,
			FiredAt:      r.FiredAt.Time,
			CLV15m:       r.Clv15m,
			CLV1h:        r.Clv1h,
			CLV6h:        r.Clv6h,
			CLV24h:       r.Clv24h,
		})
	}
	return out, nil
}

type shadowPriceAdapter struct {
	marketsRepo *repository.MarketRepository
	q           *sqlc.Queries
}

func newShadowPriceAdapter(pool *pgxpool.Pool) *shadowPriceAdapter {
	return &shadowPriceAdapter{
		marketsRepo: repository.NewMarketRepository(pool),
		q:           sqlc.New(pool),
	}
}

func (a *shadowPriceAdapter) FirstPriceAtOrAfter(ctx context.Context, conditionID, outcomeToken string, at time.Time) (float64, bool, error) {
	m, err := a.marketsRepo.GetByConditionID(ctx, conditionID)
	if err != nil {
		return 0, false, nil // condition not in DB → no price
	}
	row, err := a.q.PriceWindowStats(ctx, sqlc.PriceWindowStatsParams{
		MarketID:     m.ID,
		OutcomeToken: outcomeToken,
		Since:        tsFromTime(at),
	})
	if err != nil {
		return 0, false, err
	}
	if row.SampleCount == 0 {
		return 0, false, nil
	}
	return row.FirstPrice, true, nil
}

type shadowUpdateAdapter struct{ q *sqlc.Queries }

func newShadowUpdateAdapter(pool *pgxpool.Pool) *shadowUpdateAdapter {
	return &shadowUpdateAdapter{q: sqlc.New(pool)}
}

func (a *shadowUpdateAdapter) UpdateValues(ctx context.Context, id int64, v strategyvalue.Values) error {
	return a.q.UpdateShadowDecisionValues(ctx, sqlc.UpdateShadowDecisionValuesParams{
		ID:            id,
		Clv15m:        v.CLV15m,
		Clv1h:         v.CLV1h,
		Clv6h:         v.CLV6h,
		Clv24h:        v.CLV24h,
		OutcomeStatus: v.OutcomeStatus,
	})
}

// --- strategypromotion adapters ----------------------------------

type promotionSampleAdapter struct{ q *sqlc.Queries }

func newPromotionSampleAdapter(pool *pgxpool.Pool) *promotionSampleAdapter {
	return &promotionSampleAdapter{q: sqlc.New(pool)}
}

func (a *promotionSampleAdapter) ListPromotionSamples(ctx context.Context, lookback time.Duration) ([]strategypromotion.Sample, error) {
	since := time.Now().Add(-lookback)
	rows, err := a.q.AggregatePromotionSamples(ctx, tsFromTime(since))
	if err != nil {
		return nil, err
	}
	out := make([]strategypromotion.Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, strategypromotion.Sample{
			StrategyName:       r.StrategyName,
			StrategyVersion:    r.StrategyVersion,
			SampleSize:         int(r.SampleSize),
			MedianSignedMove6h: r.MedianSignedMove6h,
			Reversal15mRatio:   r.Reversal15mRatio,
			AlertsPerDay:       r.AlertsPerDay,
		})
	}
	return out, nil
}

// ListPromotionBucketSamples — v11.10 PART 7. Returns per-bucket
// sub-aggregates across the two diagnostic dimensions:
//   - decision_level (info / warning / critical / hard)
//   - linkage        (linked / standalone)
//
// Failure to load buckets is non-fatal at the worker layer; we still
// return an error here so the worker can log it.
func (a *promotionSampleAdapter) ListPromotionBucketSamples(ctx context.Context, lookback time.Duration) ([]strategypromotion.BucketSample, error) {
	since := tsFromTime(time.Now().Add(-lookback))
	out := make([]strategypromotion.BucketSample, 0, 64)
	dlRows, err := a.q.AggregatePromotionSamplesByDecisionLevel(ctx, since)
	if err != nil {
		return nil, err
	}
	for _, r := range dlRows {
		out = append(out, strategypromotion.BucketSample{
			StrategyName:       r.StrategyName,
			StrategyVersion:    r.StrategyVersion,
			Dimension:          "decision_level",
			Key:                r.BucketKey,
			SampleSize:         int(r.SampleSize),
			MedianSignedMove6h: r.MedianSignedMove6h,
			Reversal15mRatio:   r.Reversal15mRatio,
			AlertsPerDay:       r.AlertsPerDay,
		})
	}
	linkRows, err := a.q.AggregatePromotionSamplesByLinkage(ctx, since)
	if err != nil {
		return nil, err
	}
	for _, r := range linkRows {
		out = append(out, strategypromotion.BucketSample{
			StrategyName:       r.StrategyName,
			StrategyVersion:    r.StrategyVersion,
			Dimension:          "linkage",
			Key:                r.BucketKey,
			SampleSize:         int(r.SampleSize),
			MedianSignedMove6h: r.MedianSignedMove6h,
			Reversal15mRatio:   r.Reversal15mRatio,
			AlertsPerDay:       r.AlertsPerDay,
		})
	}
	return out, nil
}

type promotionWriterAdapter struct{ q *sqlc.Queries }

func newPromotionWriterAdapter(pool *pgxpool.Pool) *promotionWriterAdapter {
	return &promotionWriterAdapter{q: sqlc.New(pool)}
}

func (a *promotionWriterAdapter) WritePromotionReview(ctx context.Context, r strategypromotion.Review) error {
	reasons, _ := shadowdecisions.MarshalReasons(r.Reasons)
	var bucketJSON []byte
	if len(r.Buckets.ByDecisionLevel) > 0 || len(r.Buckets.ByLinkage) > 0 {
		if b, err := json.Marshal(r.Buckets); err == nil {
			bucketJSON = b
		}
	}
	return a.q.InsertStrategyPromotionReview(ctx, sqlc.InsertStrategyPromotionReviewParams{
		StrategyName:       r.StrategyName,
		StrategyVersion:    r.StrategyVersion,
		SampleSize:         int32(r.SampleSize),
		MedianSignedMove6h: r.MedianSignedMove6h,
		Reversal15mRatio:   r.Reversal15mRatio,
		AlertsPerDay:       r.AlertsPerDay,
		Eligible:           r.Eligible,
		ReasonsJson:        reasons,
		BucketDiagnostics:  bucketJSON,
		ReviewedAt:         tsFromTime(r.ReviewedAt),
	})
}

// tsFromTime is a local copy of the repository helper (kept private
// there). app-package adapters need it once for sqlc boundary calls.
func tsFromTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
