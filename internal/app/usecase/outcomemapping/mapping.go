// Package outcomemapping normalises the noisy Polymarket event-page
// market shape into a single OutcomeMapping struct downstream code
// can rely on without re-implementing the alignment rules every time.
//
// The mapping problem:
//
//   - Polymarket's event payload carries Outcomes (e.g. ["Yes","No"])
//     and OutcomePrices (e.g. ["0.62","0.38"]) parallel slices, plus
//     a CLOBTokenIDs slice that's also index-aligned. Trades / orders
//     reference the CLOB token id; annotations reference the outcome
//     label as a free-text string; predictions store an outcome
//     label too.
//   - For a binary market the "Yes" outcome at index 0 corresponds
//     to token CLOBTokenIDs[0]; "No" at index 1 to CLOBTokenIDs[1].
//   - For a multi-candidate event (Texas runoff: "Ken Paxton", "John
//     Cornyn"), each candidate is its own market (separate condition_id)
//     where the "Yes" leg = "candidate wins". The event-page payload
//     gives one market per candidate; the Outcomes slice on each
//     market is still ["Yes","No"] — the candidate name lives in
//     GroupItemTitle.
//
// Without this layer, repricing.Provider has to guess the current
// price for "Ken Paxton" by sniffing the right (condition_id,
// outcome_index) tuple — which is the bug we hit in v10.0 where 14
// markets parsed but repricing kept emitting "unclear".
//
// All resolvers are deterministic. When the inputs don't match any
// market, the resolver returns ok=false with a stable reason code
// (suitable for the Prometheus label) — NEVER an invented mapping.
package outcomemapping

import (
	"strconv"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// OutcomeMapping is the normalised view downstream code consumes.
// Every numeric field is a Polymarket-native probability (0..1) —
// callers shouldn't need to multiply by 100. IsYes / IsNo are
// canonical for binary markets so the repricing classifier can
// short-circuit the most common case.
type OutcomeMapping struct {
	EventSlug      string
	ConditionID    string
	MarketSlug     string
	Question       string
	OutcomeLabel   string
	OutcomeIndex   int
	CLOBTokenID    string
	CurrentPrice   float64
	BestBid        *float64
	BestAsk        *float64
	LastTradePrice *float64
	IsYes          bool
	IsNo           bool
	// Confidence in the mapping itself, 0..1.
	// 1.0 = exact label or token match;
	// 0.7 = group-title match (multi-candidate event);
	// 0.3 = best-effort positional fallback.
	Confidence float64
	// Reason is a stable short code emitted on the unknown metric
	// when ok=false. On a successful map it's the rule that fired:
	// "exact_label" | "case_insensitive_label" | "token_match" |
	// "group_item_title" | "binary_default" | "positional".
	Reason string
}

// Mapper holds the per-event-slug market cache and produces
// OutcomeMappings on demand. Cheap to construct (no I/O); reuse one
// instance per cycle so repeated lookups for the same event_slug
// reuse the same slice.
type Mapper struct {
	byCondition map[string][]repository.EventPageMarketRow
	byEventSlug map[string][]repository.EventPageMarketRow
	byTokenID   map[string]repository.EventPageMarketRow
}

// NewMapper builds a Mapper from a slice of event-page markets —
// usually `eventpagecontext.Summary.Markets` for a single event.
// Empty input is valid; lookups will return ok=false / unknown.
func NewMapper(markets []repository.EventPageMarketRow) *Mapper {
	m := &Mapper{
		byCondition: make(map[string][]repository.EventPageMarketRow, len(markets)),
		byEventSlug: make(map[string][]repository.EventPageMarketRow, len(markets)),
		byTokenID:   make(map[string]repository.EventPageMarketRow, len(markets)*2),
	}
	for _, mk := range markets {
		m.byCondition[mk.ConditionID] = append(m.byCondition[mk.ConditionID], mk)
		if mk.EventSlug != "" {
			m.byEventSlug[mk.EventSlug] = append(m.byEventSlug[mk.EventSlug], mk)
		}
		for _, tok := range mk.CLOBTokenIDs {
			tok = strings.TrimSpace(tok)
			if tok != "" {
				m.byTokenID[tok] = mk
			}
		}
	}
	return m
}

// ResolveByConditionAndOutcome looks up the mapping for an outcome
// label on a specific market. The label is matched case-insensitively
// against the market's Outcomes slice; on miss for a multi-candidate
// market we also try GroupItemTitle, which is where the candidate's
// real name lives (Outcomes is always ["Yes","No"] for those).
//
// On success returns the mapping + true. On miss returns the
// zero-value + false; the caller emits a metric with the Reason
// for telemetry. NEVER invents a mapping.
func (m *Mapper) ResolveByConditionAndOutcome(conditionID, outcome string) (OutcomeMapping, bool) {
	if m == nil {
		return OutcomeMapping{Reason: "nil_mapper"}, false
	}
	conditionID = strings.TrimSpace(conditionID)
	outcome = strings.TrimSpace(outcome)
	rows := m.byCondition[conditionID]
	if len(rows) == 0 {
		return OutcomeMapping{Reason: "unknown_condition_id"}, false
	}
	mk := rows[0]
	// Empty outcome label — caller wants the leading (typically
	// "Yes") outcome. Treat as positional index 0 with reduced
	// confidence so the caller can still see this happened.
	if outcome == "" {
		return mappingFromIndex(mk, 0, "binary_default", 0.6), true
	}
	// Exact label match across the Outcomes slice (case-sensitive
	// first, then case-insensitive fallback with lower confidence).
	if idx := indexOfExact(mk.Outcomes, outcome); idx >= 0 {
		return mappingFromIndex(mk, idx, "exact_label", 1.0), true
	}
	if idx := indexOfFold(mk.Outcomes, outcome); idx >= 0 {
		return mappingFromIndex(mk, idx, "case_insensitive_label", 0.85), true
	}
	// Multi-candidate event: the candidate name is in
	// GroupItemTitle and Outcomes is ["Yes","No"]. Compare on
	// GroupItemTitle and resolve to index 0 ("Yes leg").
	if equalFold(strings.TrimSpace(mk.GroupItemTitle), outcome) {
		return mappingFromIndex(mk, 0, "group_item_title", 0.7), true
	}
	// One more fallback: maybe the caller passed the candidate name
	// but the row we picked was a sibling market under the same
	// event. Walk siblings by event_slug and try GroupItemTitle.
	if mk.EventSlug != "" {
		for _, sib := range m.byEventSlug[mk.EventSlug] {
			if equalFold(strings.TrimSpace(sib.GroupItemTitle), outcome) {
				return mappingFromIndex(sib, 0, "group_item_title", 0.7), true
			}
		}
	}
	return OutcomeMapping{Reason: "label_not_found"}, false
}

// ResolveByTokenID looks up the mapping for a CLOB token id —
// the load-bearing path for trade rows whose outcome_token is the
// canonical key. Match is exact (post-TrimSpace).
func (m *Mapper) ResolveByTokenID(tokenID string) (OutcomeMapping, bool) {
	if m == nil {
		return OutcomeMapping{Reason: "nil_mapper"}, false
	}
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return OutcomeMapping{Reason: "empty_token_id"}, false
	}
	mk, ok := m.byTokenID[tokenID]
	if !ok {
		return OutcomeMapping{Reason: "unknown_token_id"}, false
	}
	// Locate the index for the matched token so we can grab the
	// right outcome label + price.
	for i, tok := range mk.CLOBTokenIDs {
		if strings.TrimSpace(tok) == tokenID {
			return mappingFromIndex(mk, i, "token_match", 1.0), true
		}
	}
	// Defensive — shouldn't reach here.
	return OutcomeMapping{Reason: "unknown_token_id"}, false
}

// ResolveByEventSlugAndAnnotationOutcome handles the
// annotation-outcome-text path. Annotations live at event_slug
// granularity (no condition_id), so we walk every market under the
// event and pick the one whose Outcomes / GroupItemTitle matches.
// First match wins; the priority is exact label → group_item_title
// → case-insensitive label.
func (m *Mapper) ResolveByEventSlugAndAnnotationOutcome(eventSlug, outcome string) (OutcomeMapping, bool) {
	if m == nil {
		return OutcomeMapping{Reason: "nil_mapper"}, false
	}
	eventSlug = strings.TrimSpace(eventSlug)
	outcome = strings.TrimSpace(outcome)
	if eventSlug == "" {
		return OutcomeMapping{Reason: "empty_event_slug"}, false
	}
	rows := m.byEventSlug[eventSlug]
	if len(rows) == 0 {
		return OutcomeMapping{Reason: "unknown_event_slug"}, false
	}
	if outcome == "" {
		// Best we can do: leading market, leading outcome.
		return mappingFromIndex(rows[0], 0, "binary_default", 0.5), true
	}
	// Pass 1: exact label match on any market.
	for _, mk := range rows {
		if idx := indexOfExact(mk.Outcomes, outcome); idx >= 0 {
			return mappingFromIndex(mk, idx, "exact_label", 1.0), true
		}
	}
	// Pass 2: group_item_title match (multi-candidate events).
	for _, mk := range rows {
		if equalFold(strings.TrimSpace(mk.GroupItemTitle), outcome) {
			return mappingFromIndex(mk, 0, "group_item_title", 0.7), true
		}
	}
	// Pass 3: case-insensitive label match.
	for _, mk := range rows {
		if idx := indexOfFold(mk.Outcomes, outcome); idx >= 0 {
			return mappingFromIndex(mk, idx, "case_insensitive_label", 0.85), true
		}
	}
	return OutcomeMapping{Reason: "label_not_found"}, false
}

// mappingFromIndex composes the canonical OutcomeMapping for one
// market row at a known outcome index.
func mappingFromIndex(mk repository.EventPageMarketRow, idx int, reason string, confidence float64) OutcomeMapping {
	out := OutcomeMapping{
		EventSlug:      mk.EventSlug,
		ConditionID:    mk.ConditionID,
		MarketSlug:     mk.MarketSlug,
		Question:       mk.Question,
		OutcomeIndex:   idx,
		LastTradePrice: mk.LastTradePrice,
		BestBid:        mk.BestBid,
		BestAsk:        mk.BestAsk,
		Confidence:     confidence,
		Reason:         reason,
	}
	if idx >= 0 && idx < len(mk.Outcomes) {
		out.OutcomeLabel = mk.Outcomes[idx]
		out.IsYes = equalFold(out.OutcomeLabel, "Yes")
		out.IsNo = equalFold(out.OutcomeLabel, "No")
	}
	if idx >= 0 && idx < len(mk.CLOBTokenIDs) {
		out.CLOBTokenID = strings.TrimSpace(mk.CLOBTokenIDs[idx])
	}
	// CurrentPrice priority: per-outcome price → lastTradePrice
	// → bestBid/Ask midpoint → 0. The per-outcome price comes
	// straight from Polymarket's OutcomePrices slice, which IS the
	// authoritative side-aware quote.
	if idx >= 0 && idx < len(mk.OutcomePrices) {
		if v, err := strconv.ParseFloat(strings.TrimSpace(mk.OutcomePrices[idx]), 64); err == nil {
			out.CurrentPrice = v
		}
	}
	if out.CurrentPrice == 0 && mk.LastTradePrice != nil {
		out.CurrentPrice = *mk.LastTradePrice
	}
	if out.CurrentPrice == 0 && mk.BestBid != nil && mk.BestAsk != nil {
		out.CurrentPrice = (*mk.BestBid + *mk.BestAsk) / 2
	}
	return out
}

// indexOfExact returns the first index in `xs` whose trimmed value
// equals `needle` exactly (case-sensitive). -1 on miss.
func indexOfExact(xs []string, needle string) int {
	for i, v := range xs {
		if strings.TrimSpace(v) == needle {
			return i
		}
	}
	return -1
}

// indexOfFold returns the first index in `xs` whose trimmed value
// equals `needle` case-insensitively. -1 on miss.
func indexOfFold(xs []string, needle string) int {
	for i, v := range xs {
		if equalFold(strings.TrimSpace(v), needle) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	return strings.EqualFold(a, b)
}
