package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// EventAnnotationRanking is the domain projection of one
// polymarket_event_annotation_rankings row.
type EventAnnotationRanking struct {
	ID                  int64
	PeriodStart         time.Time
	PeriodEnd           time.Time
	EventSlug           string
	MarketSlug          string
	AnnotationHash      string
	Rank                int32
	Importance          float64
	VolatilityPotential float64
	ProbabilityImpact   string
	AffectedOutcome     string
	Title               string
	Reason              string
	MarketRead          string
	CreatedAt           time.Time
}

// NewEventAnnotationRanking is the insert input.
type NewEventAnnotationRanking struct {
	PeriodStart         time.Time
	PeriodEnd           time.Time
	EventSlug           string
	MarketSlug          string
	AnnotationHash      string
	Rank                int32
	Importance          float64
	VolatilityPotential float64
	ProbabilityImpact   string
	AffectedOutcome     string
	Title               string
	Reason              string
	MarketRead          string
}

// DailyPoliticalIntelReport is the domain projection of one
// polymarket_daily_political_intel_reports row.
type DailyPoliticalIntelReport struct {
	ID                      int64
	ReportDate              time.Time
	PeriodStart             time.Time
	PeriodEnd               time.Time
	SelectedMarketsJSON     []byte
	SelectedAnnotationsJSON []byte
	CatalystsJSON           []byte
	AIReportText            string
	TelegramMessageIDsJSON  []byte
	DeliveryStatus          string
	LastDeliveryError       string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// NewDailyPoliticalIntelReport is the upsert input. report_date is the
// UNIQUE conflict key; an existing row for the same date refreshes
// the JSON blobs + AI text + delivery state.
type NewDailyPoliticalIntelReport struct {
	ReportDate              time.Time
	PeriodStart             time.Time
	PeriodEnd               time.Time
	SelectedMarketsJSON     []byte
	SelectedAnnotationsJSON []byte
	CatalystsJSON           []byte
	AIReportText            string
	TelegramMessageIDsJSON  []byte
	DeliveryStatus          string
	LastDeliveryError       string
}

// ErrDailyReportNotFound is returned by Get when no row exists.
var ErrDailyReportNotFound = errors.New("daily political intel report not found")

// AnnotationIntelRepository owns the v9.7 intel tables.
type AnnotationIntelRepository struct {
	q *sqlc.Queries
}

func NewAnnotationIntelRepository(pool *pgxpool.Pool) *AnnotationIntelRepository {
	return &AnnotationIntelRepository{q: sqlc.New(pool)}
}

func (r *AnnotationIntelRepository) UpsertRanking(ctx context.Context, n NewEventAnnotationRanking) error {
	return r.q.UpsertEventAnnotationRanking(ctx, sqlc.UpsertEventAnnotationRankingParams{
		PeriodStart:         tsFromTime(n.PeriodStart),
		PeriodEnd:           tsFromTime(n.PeriodEnd),
		EventSlug:           n.EventSlug,
		MarketSlug:          strPtr(n.MarketSlug),
		AnnotationHash:      n.AnnotationHash,
		Rank:                n.Rank,
		Importance:          n.Importance,
		VolatilityPotential: n.VolatilityPotential,
		ProbabilityImpact:   n.ProbabilityImpact,
		AffectedOutcome:     strPtr(n.AffectedOutcome),
		Title:               n.Title,
		Reason:              n.Reason,
		MarketRead:          n.MarketRead,
	})
}

func (r *AnnotationIntelRepository) ListRankingsForPeriod(ctx context.Context, periodStart time.Time) ([]EventAnnotationRanking, error) {
	rows, err := r.q.ListLatestRankingForPeriod(ctx, tsFromTime(periodStart))
	if err != nil {
		return nil, fmt.Errorf("list rankings for period: %w", err)
	}
	out := make([]EventAnnotationRanking, 0, len(rows))
	for _, row := range rows {
		out = append(out, rankingFromSQLC(row))
	}
	return out, nil
}

func (r *AnnotationIntelRepository) UpsertDailyReport(ctx context.Context, n NewDailyPoliticalIntelReport) (int64, error) {
	if n.DeliveryStatus == "" {
		n.DeliveryStatus = "pending"
	}
	return r.q.UpsertDailyPoliticalIntelReport(ctx, sqlc.UpsertDailyPoliticalIntelReportParams{
		ReportDate:              pgtype.Date{Time: n.ReportDate, Valid: true},
		PeriodStart:             tsFromTime(n.PeriodStart),
		PeriodEnd:               tsFromTime(n.PeriodEnd),
		SelectedMarketsJson:     n.SelectedMarketsJSON,
		SelectedAnnotationsJson: n.SelectedAnnotationsJSON,
		CatalystsJson:           n.CatalystsJSON,
		AiReportText:            n.AIReportText,
		TelegramMessageIdsJson:  n.TelegramMessageIDsJSON,
		DeliveryStatus:          n.DeliveryStatus,
		LastDeliveryError:       n.LastDeliveryError,
	})
}

func (r *AnnotationIntelRepository) GetDailyReport(ctx context.Context, day time.Time) (DailyPoliticalIntelReport, error) {
	row, err := r.q.GetDailyPoliticalIntelReport(ctx, pgtype.Date{Time: day, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DailyPoliticalIntelReport{}, ErrDailyReportNotFound
		}
		return DailyPoliticalIntelReport{}, fmt.Errorf("get daily political intel report: %w", err)
	}
	return DailyPoliticalIntelReport{
		ID:                      row.ID,
		ReportDate:              row.ReportDate.Time,
		PeriodStart:             row.PeriodStart.Time,
		PeriodEnd:               row.PeriodEnd.Time,
		SelectedMarketsJSON:     row.SelectedMarketsJson,
		SelectedAnnotationsJSON: row.SelectedAnnotationsJson,
		CatalystsJSON:           row.CatalystsJson,
		AIReportText:            row.AiReportText,
		TelegramMessageIDsJSON:  row.TelegramMessageIdsJson,
		DeliveryStatus:          row.DeliveryStatus,
		LastDeliveryError:       row.LastDeliveryError,
		CreatedAt:               row.CreatedAt.Time,
		UpdatedAt:               row.UpdatedAt.Time,
	}, nil
}

func rankingFromSQLC(row sqlc.PolymarketEventAnnotationRankings) EventAnnotationRanking {
	return EventAnnotationRanking{
		ID:                  row.ID,
		PeriodStart:         row.PeriodStart.Time,
		PeriodEnd:           row.PeriodEnd.Time,
		EventSlug:           row.EventSlug,
		MarketSlug:          derefStr(row.MarketSlug),
		AnnotationHash:      row.AnnotationHash,
		Rank:                row.Rank,
		Importance:          row.Importance,
		VolatilityPotential: row.VolatilityPotential,
		ProbabilityImpact:   row.ProbabilityImpact,
		AffectedOutcome:     derefStr(row.AffectedOutcome),
		Title:               row.Title,
		Reason:              row.Reason,
		MarketRead:          row.MarketRead,
		CreatedAt:           row.CreatedAt.Time,
	}
}
