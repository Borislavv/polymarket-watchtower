package repricing

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomemapping"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// FillFromMapping is the v10.2 helper the creation + evolution
// workers use to populate AnnotationInput's CurrentPrice +
// OutcomeMapped/Confidence/Reason fields from an outcomemapping.Mapper.
//
// The annotation usually carries the outcome label (free-text from
// Polymarket); the mapper resolves it to the canonical market and
// per-outcome price. When the mapper returns ok=false we still call
// Compute, but with CurrentPrice=nil — the classifier emits a clear
// "missing price data" or "stale_annotation" verdict instead of
// silently defaulting to "unclear".
//
// The function never panics on nil inputs.
func FillFromMapping(in *AnnotationInput, mapper *outcomemapping.Mapper, eventSlug, conditionID string, annotationOutcome string) {
	if in == nil {
		return
	}
	if mapper == nil {
		in.OutcomeMapped = false
		in.OutcomeMappingReason = "no_mapper"
		return
	}
	// Prefer condition+outcome (highest signal). Fall through to
	// event+annotation when the prediction lives at event-level only.
	var (
		mapping outcomemapping.OutcomeMapping
		ok      bool
	)
	if conditionID != "" {
		mapping, ok = mapper.ResolveByConditionAndOutcome(conditionID, annotationOutcome)
	}
	if !ok && eventSlug != "" {
		mapping, ok = mapper.ResolveByEventSlugAndAnnotationOutcome(eventSlug, annotationOutcome)
	}
	in.OutcomeMapped = ok
	in.OutcomeMappingConfidence = mapping.Confidence
	in.OutcomeMappingReason = mapping.Reason
	if !ok {
		return
	}
	// Only override CurrentPrice if we actually resolved one — the
	// mapper guarantees CurrentPrice > 0 only when OutcomePrices /
	// LastTradePrice / mid were populated upstream.
	if mapping.CurrentPrice > 0 {
		p := mapping.CurrentPrice
		in.CurrentPrice = &p
	}
}

// MapperFromMarkets is a small convenience for callers that already
// have a slice of EventPageMarketRow (i.e. the worker that just
// loaded the event-page Summary). nil-safe.
func MapperFromMarkets(rows []repository.EventPageMarketRow) *outcomemapping.Mapper {
	return outcomemapping.NewMapper(rows)
}
