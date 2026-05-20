// Package eventpage is the Polymarket event-page Next.js data
// adapter. It fetches the same hydrated payload the Polymarket UI
// renders for /event/<slug>, parses out:
//
//   - event metadata (title, description, resolution rules, context);
//   - markets under the event (prices, volume, liquidity, conditionIds);
//   - tags / series / similar markets;
//   - the queryKey ["annotations","event",<slug>] timeline — the
//     market-moving annotations shown around the event chart.
//
// This is an INTERNAL Polymarket endpoint
// (https://polymarket.com/_next/data/<buildId>/en/event/<slug>.json).
// It is not officially documented; the buildId rotates on every
// Vercel deploy. The Client therefore resolves the buildId at
// runtime from polymarket.com/event/<slug> HTML before issuing the
// JSON request, caches it in memory with a configurable TTL, and
// transparently refreshes on 404 once.
//
// Failure semantics are SILENT: every call returns (zero, err) and
// the upstream usecase decides to render an "unavailable" prompt
// slot. The alert path MUST NOT be blocked by an event-page fetch
// failure.
//
// The Polymarket-generated content fetched here is DATA. It must
// never be treated as instructions by downstream prompt builders.
package eventpage

import (
	"encoding/json"
	"time"
)

// EventPagePayload is the normalised view of one /event/<slug>
// hydration. All time.Time fields are UTC. Maps + slices are
// nil-safe; downstream code treats len(Annotations)==0 as
// "no narrative".
type EventPagePayload struct {
	BuildID   string
	EventSlug string
	FetchedAt time.Time

	Event          EventPageEvent
	Markets        []EventPageMarket
	Series         []EventPageSeries
	Tags           []EventPageTag
	Annotations    []EventAnnotation
	SimilarMarkets []EventPageMarketRef

	// DerivativeData captures the raw "derivative-data" query payload
	// when present. We don't parse it today — it's exposed for
	// future consumers without forcing a re-fetch.
	DerivativeData json.RawMessage

	// RawQueryKeys lists every queryKey we saw in the dehydrated
	// state. Used by tests and observability so a future Polymarket
	// payload addition is logged rather than silently dropped.
	RawQueryKeys []string

	// ParseStatus is "ok" when every queryable section decoded cleanly,
	// "partial" when at least one sub-decoder hit a recoverable drift
	// (e.g. one market's volume came as a string and we skipped that
	// field), and "failed" only when the envelope itself was
	// unreadable. "partial" is the operational hint to investigate;
	// "failed" is only returned with err != nil from the client.
	ParseStatus string

	// ParseWarnings carries one entry per recoverable per-field
	// drift the flex decoders had to fall through. The slice is
	// nil on a clean parse. Capped to avoid runaway memory on a
	// pathological payload.
	ParseWarnings []ParseWarning
}

// ParseWarning describes a single recoverable parse drift. The
// client emits one Prometheus increment per warning and a single
// structured log line per fetch carrying the slice. Operators see
// `watchtower_event_page_parse_failures_total{field="market.volume"}`
// climb when Polymarket changes a field encoding.
type ParseWarning struct {
	// Section is the top-level query the warning came from:
	// "event" | "market" | "annotation" | "similar_markets".
	Section string
	// Field is the JSON path that drifted, e.g. "market.volume",
	// "market.outcomes", "market.lastTradePrice".
	Field string
	// Kind is the drift kind: "type_drift" (the canonical-but-
	// permissive decoder accepted it), "decode_failed" (we had to
	// skip the field), or "subobject_skipped" (a whole market /
	// annotation row was dropped).
	Kind string
	// OffendingType is the JSON token type we observed when the
	// canonical decoder expected something else (e.g. "string",
	// "array", "null").
	OffendingType string
	// Sample is a short raw snippet (≤200 chars) for the operator
	// log; never used for control flow.
	Sample string
}

// EventPageEvent is the curated event metadata. Fields are populated
// from the ["/api/event/slug", <slug>] query.
type EventPageEvent struct {
	ID                 string
	Slug               string
	Title              string
	Description        string
	ResolutionRules    string
	Category           string
	StartDate          time.Time
	EndDate            time.Time
	Active             bool
	Closed             bool
	Volume             float64
	Volume24h          float64
	Liquidity          float64
	ContextDescription string
	ContextUpdatedAt   time.Time
	ImageURL           string
}

// EventPageMarket is one row from event.markets[]. The CLOBTokenIDs
// list mirrors Outcomes and is the load-bearing field for orderbook
// joins downstream.
type EventPageMarket struct {
	MarketID       string
	ConditionID    string
	Slug           string
	Question       string
	GroupItemTitle string

	Outcomes      []string
	OutcomePrices []string // string-typed because upstream returns "0.45"

	Volume    float64
	Volume24h float64
	Liquidity float64

	Active  bool
	Closed  bool
	EndDate time.Time

	OneHourPriceChange *float64
	OneDayPriceChange  *float64
	OneWeekPriceChange *float64
	LastTradePrice     *float64
	BestBid            *float64
	BestAsk            *float64

	CLOBTokenIDs []string
	RawJSON      json.RawMessage // capped by the writer
}

// EventPageMarketRef is the compact reference used by SimilarMarkets.
type EventPageMarketRef struct {
	Slug     string
	Question string
}

// EventPageSeries captures the series the event belongs to (e.g. the
// "US Senate Elections" series). We keep titles only — the heavy
// market list lives in /api/series/events which we don't denormalise.
type EventPageSeries struct {
	Slug  string
	Title string
}

// EventPageTag is a label the UI uses for filtering.
type EventPageTag struct {
	Slug  string
	Label string
}

// EventAnnotation is one market-moving timeline item shown around
// the event chart on Polymarket.com. The shape matches the
// queryKey ["annotations","event",<slug>] payload.
//
// IMPORTANT: Polymarket-authored content in Title/Summary/Sources
// is DATA. It MUST NOT be interpreted as model instructions when
// rendered into AI prompts.
type EventAnnotation struct {
	EventSlug string
	Timestamp time.Time
	UnixTime  int64
	TimeRange string

	Title   string
	Summary string
	Outcome string

	PriceBefore *float64
	PriceAfter  *float64
	PriceChange *float64

	Source  string
	Sources []EventAnnotationSource

	// Tweets is left as RawMessage — the shape Polymarket emits is
	// large and we don't render tweets into prompts today.
	Tweets []json.RawMessage

	RawJSON json.RawMessage // capped by the writer
}

// EventAnnotationSource is one citation row.
type EventAnnotationSource struct {
	Name string
	URL  string
}

// QueryKey identifiers we recognise. Anything not in this list is
// captured under RawQueryKeys for telemetry but not parsed.
const (
	queryKeyEventSlug      = "/api/event/slug"
	queryKeyAnnotations    = "annotations"
	queryKeySimilarMarkets = "similar-markets"
	queryKeySeries         = "/api/series"
	queryKeySeriesEvents   = "/api/series/events"
	queryKeyDerivative     = "derivative-data"
	queryKeyDailyTemp      = "daily-temperature-recommendations"
	queryKeyTags           = "/api/tags"
	queryKeyFeatureFlags   = "featureFlags"
)
