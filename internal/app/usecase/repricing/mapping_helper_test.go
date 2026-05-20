package repricing

import (
	"context"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/outcomemapping"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// noopTradesQuerier returns zero same/opposite flow so the
// classifier exercises the price-only path.
type noopTradesQuerier struct{}

func (noopTradesQuerier) sum() {}

func mkProviderForTest(t *testing.T) *Provider {
	t.Helper()
	cfg := Config{Enabled: true}
	cfg.applyDefaults()
	// Build a Provider with nil trades + nil store — the test only
	// exercises the price-classification path.
	return &Provider{
		cfg:    cfg,
		trades: nil,
		store:  nil,
	}
}

func floatPtr(v float64) *float64 { return &v }

// TestFillFromMapping_PopulatesCurrentPrice covers the canonical
// path: the mapper resolves an annotation outcome to the right
// market, FillFromMapping copies CurrentPrice into the
// AnnotationInput, and Compute returns a NON-unclear status.
func TestFillFromMapping_PopulatesCurrentPrice(t *testing.T) {
	market := repository.EventPageMarketRow{
		EventSlug:     "ev",
		ConditionID:   "0xa",
		MarketSlug:    "m",
		Question:      "q",
		Outcomes:      []string{"Yes", "No"},
		OutcomePrices: []string{"0.62", "0.38"},
		CLOBTokenIDs:  []string{"tokY", "tokN"},
	}
	mapper := outcomemapping.NewMapper([]repository.EventPageMarketRow{market})

	in := AnnotationInput{
		EventSlug:      "ev",
		ConditionID:    "0xa",
		Outcome:        "Yes",
		AnnotationHash: "h1",
		Title:          "Big news",
		Timestamp:      time.Now().Add(-1 * time.Hour),
		PriceBefore:    floatPtr(0.50),
		PriceAfter:     floatPtr(0.60),
	}
	FillFromMapping(&in, mapper, in.EventSlug, in.ConditionID, in.Outcome)

	if !in.OutcomeMapped {
		t.Fatalf("expected mapped; got %+v", in)
	}
	if in.OutcomeMappingReason != "exact_label" {
		t.Errorf("reason: got %q want exact_label", in.OutcomeMappingReason)
	}
	if in.CurrentPrice == nil || *in.CurrentPrice != 0.62 {
		t.Fatalf("CurrentPrice not filled from mapping: %+v", in)
	}
	// Drive Compute with the now-filled input.
	p := mkProviderForTest(t)
	sig, err := p.Compute(context.Background(), in, false)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.RepricingStatus == StatusUnclear {
		t.Errorf("expected non-unclear status; got %q (%s)", sig.RepricingStatus, sig.Explanation)
	}
	// With priceAfter=0.60 and currentPrice=0.62 (small same-direction
	// continuation under the overreaction threshold) the classifier
	// returns already_priced.
	if sig.RepricingStatus != StatusAlreadyPriced {
		t.Errorf("expected already_priced; got %q", sig.RepricingStatus)
	}
	if !sig.OutcomeMapped {
		t.Errorf("Signal.OutcomeMapped not propagated")
	}
}

// TestFillFromMapping_UnknownReturnsExplicitReason pins the
// no-invention rule: when the mapper can't resolve, OutcomeMapped
// stays false, the reason code is preserved, and CurrentPrice is
// not invented.
func TestFillFromMapping_UnknownReturnsExplicitReason(t *testing.T) {
	mapper := outcomemapping.NewMapper(nil)
	in := AnnotationInput{
		EventSlug:   "ev",
		ConditionID: "0xnope",
		Outcome:     "Yes",
	}
	FillFromMapping(&in, mapper, in.EventSlug, in.ConditionID, in.Outcome)
	if in.OutcomeMapped {
		t.Error("expected OutcomeMapped=false")
	}
	if in.CurrentPrice != nil {
		t.Errorf("CurrentPrice must not be invented; got %v", *in.CurrentPrice)
	}
	if in.OutcomeMappingReason == "" {
		t.Error("missing OutcomeMappingReason")
	}
}

// TestCompute_StaleAnnotation pins the v10.2 stale_annotation
// override: when the annotation is older than 2× lookback and the
// classifier would otherwise return unclear, we surface that
// explicitly so the operator sees "we have nothing fresh".
func TestCompute_StaleAnnotation(t *testing.T) {
	p := mkProviderForTest(t)
	// 2× default Lookback (24h) = 48h; pick 96h ago.
	old := time.Now().Add(-96 * time.Hour)
	in := AnnotationInput{
		EventSlug:      "ev",
		ConditionID:    "0xa",
		AnnotationHash: "h-old",
		Timestamp:      old,
		// Annotation move below MinAnnotationMove (0.05) so
		// classifier returns "unclear" first; the stale override
		// then kicks in.
		PriceBefore:  floatPtr(0.50),
		PriceAfter:   floatPtr(0.51),
		CurrentPrice: floatPtr(0.515),
	}
	sig, err := p.Compute(context.Background(), in, false)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.RepricingStatus != StatusStaleAnnotation {
		t.Errorf("expected stale_annotation; got %q (%s)", sig.RepricingStatus, sig.Explanation)
	}
}

// TestCompute_ReversedWhenPriceCrossesBack pins the existing
// reversed path under v10.2 — make sure adding mapping fields
// didn't break the classifier.
func TestCompute_ReversedWhenPriceCrossesBack(t *testing.T) {
	p := mkProviderForTest(t)
	in := AnnotationInput{
		EventSlug:                "ev",
		ConditionID:              "0xa",
		Outcome:                  "Yes",
		AnnotationHash:           "h-rev",
		Timestamp:                time.Now().Add(-2 * time.Hour),
		PriceBefore:              floatPtr(0.50),
		PriceAfter:               floatPtr(0.65),
		CurrentPrice:             floatPtr(0.45), // crossed BACK below priceBefore
		OutcomeMapped:            true,
		OutcomeMappingConfidence: 1.0,
		OutcomeMappingReason:     "exact_label",
	}
	sig, err := p.Compute(context.Background(), in, false)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.RepricingStatus != StatusReversed {
		t.Errorf("expected reversed; got %q (%s)", sig.RepricingStatus, sig.Explanation)
	}
	if !sig.OutcomeMapped || sig.OutcomeMappingConfidence != 1.0 {
		t.Errorf("mapping fields not propagated: %+v", sig)
	}
}
