package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// EventCatalystStatus enumerates the lifecycle of one catalyst.
type EventCatalystStatus string

const (
	CatalystStatusExpected    EventCatalystStatus = "expected"
	CatalystStatusActive      EventCatalystStatus = "active"
	CatalystStatusResolved    EventCatalystStatus = "resolved"
	CatalystStatusStale       EventCatalystStatus = "stale"
	CatalystStatusInvalidated EventCatalystStatus = "invalidated"
)

// EventCatalystType is an open vocabulary captured for telemetry +
// dashboards. Codebase consumers compare to known constants below
// but accept unknown values verbatim — Polymarket adds annotation
// flavours without warning.
type EventCatalystType string

const (
	CatalystTypePoll              EventCatalystType = "poll"
	CatalystTypeDebate            EventCatalystType = "debate"
	CatalystTypeRunoff            EventCatalystType = "runoff"
	CatalystTypePrimary           EventCatalystType = "primary"
	CatalystTypeEndorsement       EventCatalystType = "endorsement"
	CatalystTypeCertification     EventCatalystType = "certification"
	CatalystTypeRecount           EventCatalystType = "recount"
	CatalystTypeCourtRuling       EventCatalystType = "court_ruling"
	CatalystTypeSanctions         EventCatalystType = "sanctions"
	CatalystTypeNegotiation       EventCatalystType = "negotiation"
	CatalystTypeCeasefire         EventCatalystType = "ceasefire"
	CatalystTypeFilingDeadline    EventCatalystType = "filing_deadline"
	CatalystTypeGeopoliticalEvent EventCatalystType = "geopolitical_event"
	CatalystTypeOfficialStatement EventCatalystType = "official_statement"
	CatalystTypeElectionDay       EventCatalystType = "election_day"
	CatalystTypeOther             EventCatalystType = "other"
)

// EventCatalyst is the domain projection of one
// polymarket_event_catalysts row.
type EventCatalyst struct {
	ID                   int64
	EventSlug            string
	ConditionID          string
	CatalystType         EventCatalystType
	Title                string
	Description          string
	ExpectedAt           time.Time
	Confidence           float64
	Source               string
	SourceURL            string
	Status               EventCatalystStatus
	BullishScenario      string
	BearishScenario      string
	InvalidationScenario string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// NewEventCatalyst is the insert input. Empty optional fields default
// to "" / zero / "expected".
type NewEventCatalyst struct {
	EventSlug            string
	ConditionID          string
	CatalystType         EventCatalystType
	Title                string
	Description          string
	ExpectedAt           time.Time
	Confidence           float64
	Source               string
	SourceURL            string
	Status               EventCatalystStatus
	BullishScenario      string
	BearishScenario      string
	InvalidationScenario string
}

// EventCatalystRepository owns CRUD on polymarket_event_catalysts.
type EventCatalystRepository struct {
	q *sqlc.Queries
}

func NewEventCatalystRepository(pool *pgxpool.Pool) *EventCatalystRepository {
	return &EventCatalystRepository{q: sqlc.New(pool)}
}

// Upsert inserts a new catalyst row or refreshes mutable fields on
// (event_slug, catalyst_type, title) conflict.
func (r *EventCatalystRepository) Upsert(ctx context.Context, c NewEventCatalyst) error {
	if c.Status == "" {
		c.Status = CatalystStatusExpected
	}
	return r.q.UpsertEventCatalyst(ctx, sqlc.UpsertEventCatalystParams{
		EventSlug:            c.EventSlug,
		ConditionID:          strPtr(c.ConditionID),
		CatalystType:         string(c.CatalystType),
		Title:                c.Title,
		Description:          c.Description,
		ExpectedAt:           tsFromTime(c.ExpectedAt),
		Confidence:           c.Confidence,
		Source:               c.Source,
		SourceUrl:            c.SourceURL,
		Status:               string(c.Status),
		BullishScenario:      c.BullishScenario,
		BearishScenario:      c.BearishScenario,
		InvalidationScenario: c.InvalidationScenario,
	})
}

// ListActive returns catalysts in (expected, active) status —
// active first, then expected ordered by expected_at NULLS LAST.
func (r *EventCatalystRepository) ListActive(ctx context.Context, eventSlug string) ([]EventCatalyst, error) {
	rows, err := r.q.ListActiveEventCatalysts(ctx, eventSlug)
	if err != nil {
		return nil, fmt.Errorf("list active event catalysts: %w", err)
	}
	out := make([]EventCatalyst, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventCatalystFromSQLC(row))
	}
	return out, nil
}

// ListAll returns every catalyst (incl. resolved/stale/invalidated)
// for the event. Used by the postmortem path.
func (r *EventCatalystRepository) ListAll(ctx context.Context, eventSlug string) ([]EventCatalyst, error) {
	rows, err := r.q.ListEventCatalysts(ctx, eventSlug)
	if err != nil {
		return nil, fmt.Errorf("list event catalysts: %w", err)
	}
	out := make([]EventCatalyst, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventCatalystFromSQLC(row))
	}
	return out, nil
}

// SetStatus flips lifecycle. Used by an operator (manual override)
// or the catalyst-resolver path once AI confirms an event happened.
func (r *EventCatalystRepository) SetStatus(ctx context.Context, id int64, status EventCatalystStatus) error {
	return r.q.SetEventCatalystStatus(ctx, sqlc.SetEventCatalystStatusParams{
		ID:     id,
		Status: string(status),
	})
}

func eventCatalystFromSQLC(row sqlc.PolymarketEventCatalysts) EventCatalyst {
	return EventCatalyst{
		ID:                   row.ID,
		EventSlug:            row.EventSlug,
		ConditionID:          derefStr(row.ConditionID),
		CatalystType:         EventCatalystType(row.CatalystType),
		Title:                row.Title,
		Description:          row.Description,
		ExpectedAt:           tsTime(row.ExpectedAt),
		Confidence:           row.Confidence,
		Source:               row.Source,
		SourceURL:            row.SourceUrl,
		Status:               EventCatalystStatus(row.Status),
		BullishScenario:      row.BullishScenario,
		BearishScenario:      row.BearishScenario,
		InvalidationScenario: row.InvalidationScenario,
		CreatedAt:            row.CreatedAt.Time,
		UpdatedAt:            row.UpdatedAt.Time,
	}
}
