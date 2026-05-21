// Package repository wraps the sqlc-generated DB layer with domain
// types. This file owns the Polymarket event-page tables.
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// EventPageRepository owns reads + writes for the
// polymarket_event_page_* and polymarket_event_annotations tables.
type EventPageRepository struct {
	q *sqlc.Queries
}

// NewEventPageRepository wires the repo.
func NewEventPageRepository(pool *pgxpool.Pool) *EventPageRepository {
	return &EventPageRepository{q: sqlc.New(pool)}
}

// --- domain projections ---------------------------------------------------

// NewEventPageSnapshot is the insert input for one fetch.
type NewEventPageSnapshot struct {
	EventSlug string
	BuildID   string
	FetchedAt time.Time
	RawJSON   []byte
}

// NewEventPageMarket is the insert input for one row in
// polymarket_event_page_markets.
type NewEventPageMarket struct {
	SnapshotID         int64
	EventSlug          string
	MarketID           string
	ConditionID        string
	MarketSlug         string
	Question           string
	GroupItemTitle     string
	Outcomes           []string
	OutcomePrices      []string
	Volume             float64
	Volume24h          float64
	Liquidity          float64
	Active             bool
	Closed             bool
	EndDate            time.Time
	OneHourPriceChange *float64
	OneDayPriceChange  *float64
	OneWeekPriceChange *float64
	LastTradePrice     *float64
	BestBid            *float64
	BestAsk            *float64
	CLOBTokenIDs       []string
	RawJSON            []byte
}

// NewEventAnnotation is the insert input for one annotation row.
type NewEventAnnotation struct {
	EventSlug   string
	ItemHash    string
	Timestamp   time.Time
	UnixTime    int64
	TimeRange   string
	Title       string
	Summary     string
	Outcome     string
	PriceBefore *float64
	PriceAfter  *float64
	PriceChange *float64
	Source      string
	SourcesJSON []byte
	TweetsJSON  []byte
	RawJSON     []byte
}

// EventAnnotation is the read shape consumed by the AI context
// renderer + lag detector.
type EventAnnotation struct {
	ID          int64
	EventSlug   string
	ItemHash    string
	Timestamp   time.Time
	UnixTime    int64
	TimeRange   string
	Title       string
	Summary     string
	Outcome     string
	PriceBefore *float64
	PriceAfter  *float64
	PriceChange *float64
	Source      string
	SourcesJSON []byte
	TweetsJSON  []byte
	RawJSON     []byte
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// EventPageMarketRow is the read shape for one denormalised market.
type EventPageMarketRow struct {
	ID                 int64
	SnapshotID         int64
	EventSlug          string
	MarketID           string
	ConditionID        string
	MarketSlug         string
	Question           string
	GroupItemTitle     string
	Outcomes           []string
	OutcomePrices      []string
	Volume             float64
	Volume24h          float64
	Liquidity          float64
	Active             bool
	Closed             bool
	EndDate            time.Time
	OneHourPriceChange *float64
	OneDayPriceChange  *float64
	OneWeekPriceChange *float64
	LastTradePrice     *float64
	BestBid            *float64
	BestAsk            *float64
	CLOBTokenIDs       []string
}

// EventPageFetchState is the read shape for one per-event refresh row.
type EventPageFetchState struct {
	EventSlug       string
	LastFetchedAt   time.Time
	LastSuccessAt   time.Time
	LastError       string
	LastBuildID     string
	LastAnnotations int32
	UpdatedAt       time.Time
}

// ErrEventPageStateNotFound is returned by FetchState when no row
// exists for the event yet.
var ErrEventPageStateNotFound = errors.New("event page fetch state not found")

// --- write path -----------------------------------------------------------

// rawJSONCap caps a payload before it lands in JSONB. The migration
// comment commits to "writer-capped raw JSON" — this is that point.
const rawJSONCap = 1 << 20 // 1 MB

// InsertSnapshot creates the audit row that owns the per-fetch
// market rows via FK. Returns the new snapshot id.
func (r *EventPageRepository) InsertSnapshot(ctx context.Context, s NewEventPageSnapshot) (int64, error) {
	raw := capRaw(s.RawJSON, rawJSONCap)
	hash := sha256.Sum256(raw)
	return r.q.InsertEventPageSnapshot(ctx, sqlc.InsertEventPageSnapshotParams{
		EventSlug: s.EventSlug,
		BuildID:   s.BuildID,
		FetchedAt: tsFromTime(s.FetchedAt),
		RawHash:   hex.EncodeToString(hash[:]),
		RawJson:   raw,
	})
}

// InsertMarket writes one row.
func (r *EventPageRepository) InsertMarket(ctx context.Context, m NewEventPageMarket) error {
	outcomesJSON, _ := json.Marshal(m.Outcomes)
	pricesJSON, _ := json.Marshal(m.OutcomePrices)
	tokensJSON, _ := json.Marshal(m.CLOBTokenIDs)
	return r.q.InsertEventPageMarket(ctx, sqlc.InsertEventPageMarketParams{
		SnapshotID:         m.SnapshotID,
		EventSlug:          m.EventSlug,
		MarketID:           m.MarketID,
		ConditionID:        m.ConditionID,
		MarketSlug:         m.MarketSlug,
		Question:           m.Question,
		GroupItemTitle:     strPtr(m.GroupItemTitle),
		OutcomesJson:       outcomesJSON,
		OutcomePricesJson:  pricesJSON,
		Volume:             m.Volume,
		Volume24h:          m.Volume24h,
		Liquidity:          m.Liquidity,
		Active:             m.Active,
		Closed:             m.Closed,
		EndDate:            tsFromTime(m.EndDate),
		OneHourPriceChange: m.OneHourPriceChange,
		OneDayPriceChange:  m.OneDayPriceChange,
		OneWeekPriceChange: m.OneWeekPriceChange,
		LastTradePrice:     m.LastTradePrice,
		BestBid:            m.BestBid,
		BestAsk:            m.BestAsk,
		ClobTokenIdsJson:   tokensJSON,
		RawJson:            capRaw(m.RawJSON, rawJSONCap),
	})
}

// UpsertAnnotation inserts/updates one annotation idempotently keyed
// on (event_slug, item_hash).
func (r *EventPageRepository) UpsertAnnotation(ctx context.Context, a NewEventAnnotation) error {
	return r.q.UpsertEventAnnotation(ctx, sqlc.UpsertEventAnnotationParams{
		EventSlug:   a.EventSlug,
		ItemHash:    a.ItemHash,
		Timestamp:   tsFromTime(a.Timestamp),
		UnixTime:    a.UnixTime,
		TimeRange:   strPtr(a.TimeRange),
		Title:       a.Title,
		Summary:     strPtr(a.Summary),
		Outcome:     strPtr(a.Outcome),
		PriceBefore: a.PriceBefore,
		PriceAfter:  a.PriceAfter,
		PriceChange: a.PriceChange,
		Source:      strPtr(a.Source),
		SourcesJson: capRaw(a.SourcesJSON, rawJSONCap),
		TweetsJson:  capRaw(a.TweetsJSON, rawJSONCap),
		RawJson:     capRaw(a.RawJSON, rawJSONCap),
	})
}

// FetchState returns the recorded refresh metadata or
// ErrEventPageStateNotFound when the event has never been fetched.
func (r *EventPageRepository) FetchState(ctx context.Context, eventSlug string) (EventPageFetchState, error) {
	row, err := r.q.GetEventPageFetchState(ctx, eventSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventPageFetchState{}, ErrEventPageStateNotFound
		}
		return EventPageFetchState{}, fmt.Errorf("get event page fetch state: %w", err)
	}
	return EventPageFetchState{
		EventSlug:       row.EventSlug,
		LastFetchedAt:   row.LastFetchedAt.Time,
		LastSuccessAt:   tsTime(row.LastSuccessAt),
		LastError:       derefStr(row.LastError),
		LastBuildID:     derefStr(row.LastBuildID),
		LastAnnotations: row.LastAnnotations,
		UpdatedAt:       row.UpdatedAt.Time,
	}, nil
}

// MarkFetch records one fetch attempt. fetchErr is empty on success.
// buildID is the resolved id we used (or "" when resolver failed).
func (r *EventPageRepository) MarkFetch(ctx context.Context, eventSlug string, fetchedAt time.Time, buildID string, annCount int32, fetchErr string) error {
	successAt := time.Time{}
	if fetchErr == "" {
		successAt = fetchedAt
	}
	return r.q.UpsertEventPageFetchState(ctx, sqlc.UpsertEventPageFetchStateParams{
		EventSlug:       eventSlug,
		LastFetchedAt:   tsFromTime(fetchedAt),
		LastSuccessAt:   tsFromTime(successAt),
		LastError:       strPtr(fetchErr),
		LastBuildID:     strPtr(buildID),
		LastAnnotations: annCount,
	})
}

// --- read path ------------------------------------------------------------

// ListRecentAnnotations returns the most recent annotations for an
// event, newest first by timestamp (NULL-last).
func (r *EventPageRepository) ListRecentAnnotations(ctx context.Context, eventSlug string, limit int32) ([]EventAnnotation, error) {
	rows, err := r.q.ListRecentEventAnnotations(ctx, sqlc.ListRecentEventAnnotationsParams{
		EventSlug:  eventSlug,
		LimitCount: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list recent event annotations: %w", err)
	}
	out := make([]EventAnnotation, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventAnnotationFromSQLC(row))
	}
	return out, nil
}

// ListLatestEventMarkets returns the newest market row per market_id
// for the event. Used by the AI context renderer + lag detector.
func (r *EventPageRepository) ListLatestEventMarkets(ctx context.Context, eventSlug string) ([]EventPageMarketRow, error) {
	rows, err := r.q.ListLatestEventPageMarkets(ctx, eventSlug)
	if err != nil {
		return nil, fmt.Errorf("list latest event page markets: %w", err)
	}
	out := make([]EventPageMarketRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventPageMarketRowFromSQLC(row))
	}
	return out, nil
}

// --- conversions ----------------------------------------------------------

func eventAnnotationFromSQLC(row sqlc.PolymarketEventAnnotations) EventAnnotation {
	return EventAnnotation{
		ID:          row.ID,
		EventSlug:   row.EventSlug,
		ItemHash:    row.ItemHash,
		Timestamp:   tsTime(row.Timestamp),
		UnixTime:    row.UnixTime,
		TimeRange:   derefStr(row.TimeRange),
		Title:       row.Title,
		Summary:     derefStr(row.Summary),
		Outcome:     derefStr(row.Outcome),
		PriceBefore: row.PriceBefore,
		PriceAfter:  row.PriceAfter,
		PriceChange: row.PriceChange,
		Source:      derefStr(row.Source),
		SourcesJSON: row.SourcesJson,
		TweetsJSON:  row.TweetsJson,
		RawJSON:     row.RawJson,
		FirstSeenAt: row.FirstSeenAt.Time,
		LastSeenAt:  row.LastSeenAt.Time,
	}
}

func eventPageMarketRowFromSQLC(row sqlc.PolymarketEventPageMarkets) EventPageMarketRow {
	out := EventPageMarketRow{
		ID:                 row.ID,
		SnapshotID:         row.SnapshotID,
		EventSlug:          row.EventSlug,
		MarketID:           row.MarketID,
		ConditionID:        row.ConditionID,
		MarketSlug:         row.MarketSlug,
		Question:           row.Question,
		GroupItemTitle:     derefStr(row.GroupItemTitle),
		Volume:             row.Volume,
		Volume24h:          row.Volume24h,
		Liquidity:          row.Liquidity,
		Active:             row.Active,
		Closed:             row.Closed,
		EndDate:            tsTime(row.EndDate),
		OneHourPriceChange: row.OneHourPriceChange,
		OneDayPriceChange:  row.OneDayPriceChange,
		OneWeekPriceChange: row.OneWeekPriceChange,
		LastTradePrice:     row.LastTradePrice,
		BestBid:            row.BestBid,
		BestAsk:            row.BestAsk,
	}
	_ = json.Unmarshal(row.OutcomesJson, &out.Outcomes)
	_ = json.Unmarshal(row.OutcomePricesJson, &out.OutcomePrices)
	_ = json.Unmarshal(row.ClobTokenIdsJson, &out.CLOBTokenIDs)
	return out
}

// capRaw bounds a JSON blob at n bytes. Polymarket payloads are
// generally well under 1 MB but a defensive cap protects the DB.
func capRaw(b []byte, n int) []byte {
	if n <= 0 || len(b) <= n {
		return b
	}
	out := make([]byte, n)
	copy(out, b[:n])
	return out
}

// --- v10.5 canonical-slug aliases ----------------------------------------

// UpsertEventSlugAlias persists a (original → canonical) slug
// mapping discovered by the eventpage client when Polymarket
// 307-redirects an event JSON URL to a different /event/<slug> page.
func (r *EventPageRepository) UpsertEventSlugAlias(ctx context.Context, original, canonical, source string) error {
	return r.q.UpsertEventSlugAlias(ctx, sqlc.UpsertEventSlugAliasParams{
		OriginalSlug:  original,
		CanonicalSlug: canonical,
		Source:        source,
	})
}

// GetEventSlugAlias returns the canonical slug for `original` when
// the client has previously observed a 307 → /event/<canonical>
// redirect. (zero, false, nil) on miss; never returns a transport
// error to the caller — fail-open is the contract.
func (r *EventPageRepository) GetEventSlugAlias(ctx context.Context, original string) (string, bool, error) {
	canonical, err := r.q.GetEventSlugAlias(ctx, original)
	if err != nil {
		// pgx.ErrNoRows / connection blip → fail-open.
		return "", false, nil
	}
	if canonical == "" {
		return "", false, nil
	}
	return canonical, true, nil
}
