package eventpage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxParseWarnings caps the warning slice so a pathological payload
// (e.g. Polymarket renames every field in a deploy) doesn't grow
// memory unboundedly. The operator gets a representative sample;
// metrics still increment for every drift.
const maxParseWarnings = 64

// parsePayload normalises the Next.js dehydrated state into our
// EventPagePayload. The shape we accept:
//
//	{
//	  "pageProps": {
//	    "dehydratedState": {
//	      "queries": [
//	        {"queryKey": ["/api/event/slug", "<slug>"], "state": {"data": {...}}},
//	        {"queryKey": ["annotations", "event", "<slug>"], "state": {"data": [ ... ]}},
//	        ...
//	      ]
//	    }
//	  }
//	}
//
// Unknown queryKeys are surfaced under RawQueryKeys but never cause
// parse failure — Polymarket adds fields without warning.
//
// Polymarket-authored strings (titles, summaries, source names) are
// stored verbatim; they are DATA, not instructions.
func parsePayload(eventSlug string, raw []byte, now time.Time) (*EventPagePayload, error) {
	var envelope struct {
		PageProps struct {
			DehydratedState struct {
				Queries []dehydratedQuery `json:"queries"`
			} `json:"dehydratedState"`
		} `json:"pageProps"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("eventpage: parse envelope: %w", err)
	}
	out := &EventPagePayload{
		EventSlug: eventSlug,
		FetchedAt: now.UTC(),
	}
	for _, q := range envelope.PageProps.DehydratedState.Queries {
		key := stringifyQueryKey(q.QueryKey)
		out.RawQueryKeys = append(out.RawQueryKeys, key)
		switch {
		case len(q.QueryKey) >= 1 && q.QueryKey[0].asString() == queryKeyEventSlug:
			ev, markets, ws, err := parseEventQuery(q.State.Data)
			if err == nil {
				out.Event = ev
				out.Markets = markets
			}
			out.ParseWarnings = appendCapped(out.ParseWarnings, ws)
		case len(q.QueryKey) >= 3 &&
			q.QueryKey[0].asString() == queryKeyAnnotations &&
			q.QueryKey[1].asString() == "event":
			out.Annotations = parseAnnotations(eventSlug, q.State.Data)
		case len(q.QueryKey) >= 1 && q.QueryKey[0].asString() == queryKeySimilarMarkets:
			out.SimilarMarkets = parseSimilarMarkets(q.State.Data)
		case len(q.QueryKey) >= 1 && q.QueryKey[0].asString() == queryKeySeries:
			out.Series = append(out.Series, parseSeries(q.QueryKey, q.State.Data)...)
		case len(q.QueryKey) >= 1 && q.QueryKey[0].asString() == queryKeyTags:
			out.Tags = append(out.Tags, parseTags(q.State.Data)...)
		case len(q.QueryKey) >= 1 && q.QueryKey[0].asString() == queryKeyDerivative:
			out.DerivativeData = append(json.RawMessage(nil), q.State.Data...)
		}
	}
	if len(out.ParseWarnings) == 0 {
		out.ParseStatus = "ok"
	} else {
		out.ParseStatus = "partial"
	}
	return out, nil
}

// appendCapped appends `add` to `dst` up to maxParseWarnings.
func appendCapped(dst, add []ParseWarning) []ParseWarning {
	for _, w := range add {
		if len(dst) >= maxParseWarnings {
			return dst
		}
		dst = append(dst, w)
	}
	return dst
}

// dehydratedQuery is the shape of one entry in queries[].
type dehydratedQuery struct {
	QueryKey []rawJSON  `json:"queryKey"`
	State    queryState `json:"state"`
}

type queryState struct {
	Data json.RawMessage `json:"data"`
}

// rawJSON is a JSON value that may be a string, number, bool, or
// nested object/array. We only ever read it as a string via
// asString().
type rawJSON struct {
	raw json.RawMessage
}

func (r *rawJSON) UnmarshalJSON(b []byte) error {
	r.raw = append(r.raw[:0], b...)
	return nil
}

func (r rawJSON) asString() string {
	if len(r.raw) == 0 {
		return ""
	}
	// String literal: strip quotes.
	if r.raw[0] == '"' {
		var s string
		if err := json.Unmarshal(r.raw, &s); err == nil {
			return s
		}
	}
	return strings.TrimSpace(string(r.raw))
}

func stringifyQueryKey(parts []rawJSON) string {
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(p.asString())
	}
	return b.String()
}

// --- event query ---------------------------------------------------------

// eventQueryShape mirrors the relevant fields of the
// ["/api/event/slug",<slug>] payload. Many fields are optional;
// Polymarket adds new ones over time and we ignore everything we
// don't consume.
// eventQueryShape uses flex* numeric types so an event-level field
// drift doesn't poison the whole event decode. Markets are
// deliberately decoded as []json.RawMessage so a single broken market
// row can be skipped without nuking the rest of the array — see
// parseEventQuery.
type eventQueryShape struct {
	ID                 string            `json:"id"`
	Slug               string            `json:"slug"`
	Title              string            `json:"title"`
	Description        string            `json:"description"`
	ResolutionSource   string            `json:"resolutionSource"`
	ResolutionRules    string            `json:"resolutionRules"`
	Category           string            `json:"category"`
	StartDate          string            `json:"startDate"`
	EndDate            string            `json:"endDate"`
	Active             bool              `json:"active"`
	Closed             bool              `json:"closed"`
	Volume             flexFloat64       `json:"volume"`
	Volume24h          flexFloat64       `json:"volume24hr"`
	Liquidity          flexFloat64       `json:"liquidity"`
	ContextDescription string            `json:"context_description"`
	ContextUpdatedAt   string            `json:"context_updated_at"`
	Image              string            `json:"image"`
	Markets            []json.RawMessage `json:"markets"`
}

// marketQueryShape uses the flex* tolerant types so that one market's
// drifted field encoding never wins the race against the whole
// markets[] decode. See flex.go for the per-field rationale; the
// short version is Polymarket frequently mixes float/string and
// array/json-encoded-string for the same field across markets.
type marketQueryShape struct {
	ID                 string         `json:"id"`
	ConditionID        string         `json:"conditionId"`
	Slug               string         `json:"slug"`
	Question           string         `json:"question"`
	GroupItemTitle     string         `json:"groupItemTitle"`
	Outcomes           flexStringList `json:"outcomes"`
	OutcomePrices      flexStringList `json:"outcomePrices"`
	Volume             flexFloat64    `json:"volume"`
	Volume24h          flexFloat64    `json:"volume24hr"`
	Liquidity          flexFloat64    `json:"liquidity"`
	Active             bool           `json:"active"`
	Closed             bool           `json:"closed"`
	EndDate            string         `json:"endDate"`
	OneHourPriceChange flexFloat64Ptr `json:"oneHourPriceChange"`
	OneDayPriceChange  flexFloat64Ptr `json:"oneDayPriceChange"`
	OneWeekPriceChange flexFloat64Ptr `json:"oneWeekPriceChange"`
	LastTradePrice     flexFloat64Ptr `json:"lastTradePrice"`
	BestBid            flexFloat64Ptr `json:"bestBid"`
	BestAsk            flexFloat64Ptr `json:"bestAsk"`
	ClobTokenIDs       flexStringList `json:"clobTokenIds"`
}

// parseEventQuery decodes the ["/api/event/slug",<slug>] payload.
//
// Markets are isolated: each market's RawJSON is unmarshalled
// individually, so a single drifted field on market[3] does NOT
// destroy markets[0..2,4..n]. Per-market failures append a
// ParseWarning of kind "subobject_skipped"; per-field flex drifts
// append "type_drift". The returned warning slice is appended to the
// caller's accumulator at the parsePayload level.
func parseEventQuery(data json.RawMessage) (EventPageEvent, []EventPageMarket, []ParseWarning, error) {
	if len(data) == 0 {
		return EventPageEvent{}, nil, nil, nil
	}
	var ev eventQueryShape
	if err := json.Unmarshal(data, &ev); err != nil {
		// The envelope itself is unreadable — surface a single
		// section-level warning and return what we have. Annotations
		// + similar markets remain available because they live in
		// sibling queries.
		return EventPageEvent{}, nil, []ParseWarning{{
			Section:       "event",
			Field:         "event",
			Kind:          "decode_failed",
			OffendingType: jsonTopType(data),
			Sample:        sampleSnippet(data),
		}}, fmt.Errorf("event slug parse: %w", err)
	}
	var warnings []ParseWarning
	recordFlexFloat(&warnings, "event", "event.volume", ev.Volume, data, "volume")
	recordFlexFloat(&warnings, "event", "event.volume24hr", ev.Volume24h, data, "volume24hr")
	recordFlexFloat(&warnings, "event", "event.liquidity", ev.Liquidity, data, "liquidity")

	out := EventPageEvent{
		ID:                 ev.ID,
		Slug:               ev.Slug,
		Title:              ev.Title,
		Description:        firstNonEmpty(ev.Description, ev.ResolutionSource),
		ResolutionRules:    ev.ResolutionRules,
		Category:           ev.Category,
		StartDate:          parseTimeOrZero(ev.StartDate),
		EndDate:            parseTimeOrZero(ev.EndDate),
		Active:             ev.Active,
		Closed:             ev.Closed,
		Volume:             ev.Volume.Float64(),
		Volume24h:          ev.Volume24h.Float64(),
		Liquidity:          ev.Liquidity.Float64(),
		ContextDescription: ev.ContextDescription,
		ContextUpdatedAt:   parseTimeOrZero(ev.ContextUpdatedAt),
		ImageURL:           ev.Image,
	}
	markets := make([]EventPageMarket, 0, len(ev.Markets))
	for i, raw := range ev.Markets {
		mm, w, err := parseOneMarket(raw)
		if err != nil {
			warnings = append(warnings, ParseWarning{
				Section:       "market",
				Field:         fmt.Sprintf("markets[%d]", i),
				Kind:          "subobject_skipped",
				OffendingType: jsonTopType(raw),
				Sample:        sampleSnippet(raw),
			})
			continue
		}
		warnings = append(warnings, w...)
		markets = append(markets, mm)
	}
	return out, markets, warnings, nil
}

// parseOneMarket decodes a single market row in isolation. The
// per-market RawJSON is preserved verbatim so downstream consumers
// can re-parse if they need a field we don't surface yet.
func parseOneMarket(raw json.RawMessage) (EventPageMarket, []ParseWarning, error) {
	var m marketQueryShape
	if err := json.Unmarshal(raw, &m); err != nil {
		return EventPageMarket{}, nil, err
	}
	var ws []ParseWarning
	recordFlexFloat(&ws, "market", "market.volume", m.Volume, raw, "volume")
	recordFlexFloat(&ws, "market", "market.volume24hr", m.Volume24h, raw, "volume24hr")
	recordFlexFloat(&ws, "market", "market.liquidity", m.Liquidity, raw, "liquidity")
	recordFlexStringList(&ws, "market", "market.outcomes", m.Outcomes, raw, "outcomes")
	recordFlexStringList(&ws, "market", "market.outcomePrices", m.OutcomePrices, raw, "outcomePrices")
	recordFlexStringList(&ws, "market", "market.clobTokenIds", m.ClobTokenIDs, raw, "clobTokenIds")
	mm := EventPageMarket{
		MarketID:           m.ID,
		ConditionID:        m.ConditionID,
		Slug:               m.Slug,
		Question:           m.Question,
		GroupItemTitle:     m.GroupItemTitle,
		Outcomes:           m.Outcomes.Vals(),
		OutcomePrices:      m.OutcomePrices.Vals(),
		Volume:             m.Volume.Float64(),
		Volume24h:          m.Volume24h.Float64(),
		Liquidity:          m.Liquidity.Float64(),
		Active:             m.Active,
		Closed:             m.Closed,
		EndDate:            parseTimeOrZero(m.EndDate),
		OneHourPriceChange: m.OneHourPriceChange.Ptr(),
		OneDayPriceChange:  m.OneDayPriceChange.Ptr(),
		OneWeekPriceChange: m.OneWeekPriceChange.Ptr(),
		LastTradePrice:     m.LastTradePrice.Ptr(),
		BestBid:            m.BestBid.Ptr(),
		BestAsk:            m.BestAsk.Ptr(),
		CLOBTokenIDs:       m.ClobTokenIDs.Vals(),
		RawJSON:            append(json.RawMessage(nil), raw...),
	}
	return mm, ws, nil
}

// recordFlexFloat appends a type_drift warning when the flexFloat64
// took the string fallback. No-op on clean (number) decodes.
func recordFlexFloat(ws *[]ParseWarning, section, field string, f flexFloat64, raw json.RawMessage, key string) {
	if !f.Drifted() {
		return
	}
	*ws = append(*ws, ParseWarning{
		Section:       section,
		Field:         field,
		Kind:          "type_drift",
		OffendingType: "string",
		Sample:        snippetField(raw, key),
	})
}

// recordFlexStringList appends a type_drift warning when the
// flexStringList took the encoded-string fallback.
func recordFlexStringList(ws *[]ParseWarning, section, field string, l flexStringList, raw json.RawMessage, key string) {
	if !l.Drifted() {
		return
	}
	*ws = append(*ws, ParseWarning{
		Section:       section,
		Field:         field,
		Kind:          "type_drift",
		OffendingType: "encoded_string",
		Sample:        snippetField(raw, key),
	})
}

// jsonTopType returns a short human label for the top-level JSON
// token kind of `b`. Used only for warning telemetry.
func jsonTopType(b json.RawMessage) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return "missing"
	}
	switch b[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// sampleSnippet returns up to 200 chars of raw JSON for the operator
// log. Trimmed and best-effort; never used for control flow.
func sampleSnippet(b json.RawMessage) string {
	s := string(bytes.TrimSpace(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// snippetField pulls the raw value of one key from a JSON object for
// the warning log. Best-effort — returns "" if the key isn't found.
func snippetField(raw json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m[key]; ok {
		return sampleSnippet(v)
	}
	return ""
}

// --- annotations ---------------------------------------------------------

// annotationShape mirrors one row in the annotations query payload.
// We keep the parser permissive (most fields optional) because the
// Polymarket UI ships rows with mixed completeness.
type annotationShape struct {
	Timestamp   string          `json:"timestamp"`
	UnixTime    int64           `json:"unixTime"`
	TimeRange   string          `json:"timeRange"`
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	Outcome     string          `json:"outcome"`
	PriceBefore *float64        `json:"priceBefore"`
	PriceAfter  *float64        `json:"priceAfter"`
	PriceChange *float64        `json:"priceChange"`
	Source      string          `json:"source"`
	Sources     []sourceShape   `json:"sources"`
	Tweets      json.RawMessage `json:"tweets"`
}

type sourceShape struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func parseAnnotations(eventSlug string, data json.RawMessage) []EventAnnotation {
	if len(data) == 0 {
		return nil
	}
	var rows []annotationShape
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil
	}
	out := make([]EventAnnotation, 0, len(rows))
	for _, r := range rows {
		ann := EventAnnotation{
			EventSlug:   eventSlug,
			Timestamp:   parseTimeOrZero(r.Timestamp),
			UnixTime:    r.UnixTime,
			TimeRange:   r.TimeRange,
			Title:       strings.TrimSpace(r.Title),
			Summary:     strings.TrimSpace(r.Summary),
			Outcome:     strings.TrimSpace(r.Outcome),
			PriceBefore: r.PriceBefore,
			PriceAfter:  r.PriceAfter,
			PriceChange: r.PriceChange,
			Source:      strings.TrimSpace(r.Source),
		}
		// If unixTime is set but timestamp parsing failed, derive
		// the time from unixTime so downstream code always has a
		// usable instant.
		if ann.Timestamp.IsZero() && r.UnixTime > 0 {
			ann.Timestamp = time.Unix(r.UnixTime, 0).UTC()
		}
		for _, s := range r.Sources {
			ann.Sources = append(ann.Sources, EventAnnotationSource{Name: s.Name, URL: s.URL})
		}
		// Preserve tweets as raw fragments. We cap upstream.
		if len(r.Tweets) > 0 && string(r.Tweets) != "null" {
			ann.Tweets = []json.RawMessage{append(json.RawMessage(nil), r.Tweets...)}
		}
		buf, _ := json.Marshal(r)
		ann.RawJSON = buf
		out = append(out, ann)
	}
	return out
}

// AnnotationHash is the dedup key for one annotation, stable across
// re-fetches. Built from (event_slug, unix_time, outcome, title).
// Time is omitted from the hash because Polymarket sometimes back-
// fills timestamps on the same logical item.
func AnnotationHash(a EventAnnotation) string {
	t := strconv.FormatInt(a.UnixTime, 10)
	src := a.EventSlug + "|" + t + "|" + a.Outcome + "|" + a.Title
	h := sha256.Sum256([]byte(src))
	return hex.EncodeToString(h[:16])
}

// --- similar markets / series / tags ------------------------------------

func parseSimilarMarkets(data json.RawMessage) []EventPageMarketRef {
	if len(data) == 0 {
		return nil
	}
	// Two observed shapes: an array of {slug,question} OR
	// {"data":[...]} wrapper. Try both.
	var direct []struct {
		Slug     string `json:"slug"`
		Question string `json:"question"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(data, &direct); err == nil && len(direct) > 0 {
		return refsFromStruct(direct)
	}
	var wrapper struct {
		Data []struct {
			Slug     string `json:"slug"`
			Question string `json:"question"`
			Title    string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		return refsFromStruct(wrapper.Data)
	}
	return nil
}

func refsFromStruct(rows []struct {
	Slug     string `json:"slug"`
	Question string `json:"question"`
	Title    string `json:"title"`
}) []EventPageMarketRef {
	out := make([]EventPageMarketRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, EventPageMarketRef{
			Slug:     r.Slug,
			Question: firstNonEmpty(r.Question, r.Title),
		})
	}
	return out
}

func parseSeries(key []rawJSON, data json.RawMessage) []EventPageSeries {
	if len(data) == 0 {
		return nil
	}
	var s struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &s); err == nil && s.Slug != "" {
		return []EventPageSeries{{Slug: s.Slug, Title: s.Title}}
	}
	// Fall back: queryKey may carry the slug.
	if len(key) >= 2 {
		slug := key[1].asString()
		if slug != "" {
			return []EventPageSeries{{Slug: slug}}
		}
	}
	return nil
}

func parseTags(data json.RawMessage) []EventPageTag {
	if len(data) == 0 {
		return nil
	}
	var rows []struct {
		Slug  string `json:"slug"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil
	}
	out := make([]EventPageTag, 0, len(rows))
	for _, r := range rows {
		out = append(out, EventPageTag{Slug: r.Slug, Label: r.Label})
	}
	return out
}

// --- helpers -------------------------------------------------------------

func parseTimeOrZero(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func decodeStringArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// Some Polymarket payloads escape the outer quotes. Try
		// unwrapping once.
		var unwrapped string
		if err2 := json.Unmarshal([]byte(s), &unwrapped); err2 == nil {
			if err3 := json.Unmarshal([]byte(unwrapped), &out); err3 == nil {
				return out
			}
		}
		return nil
	}
	return out
}

func firstNonEmpty(opts ...string) string {
	for _, o := range opts {
		if strings.TrimSpace(o) != "" {
			return o
		}
	}
	return ""
}
