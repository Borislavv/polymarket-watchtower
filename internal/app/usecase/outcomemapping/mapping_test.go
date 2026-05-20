package outcomemapping

import (
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func mkMarket(slug, conditionID, group string, outcomes, prices, tokens []string) repository.EventPageMarketRow {
	return repository.EventPageMarketRow{
		EventSlug:      slug,
		ConditionID:    conditionID,
		MarketSlug:     "m-" + conditionID,
		Question:       "q",
		GroupItemTitle: group,
		Outcomes:       outcomes,
		OutcomePrices:  prices,
		CLOBTokenIDs:   tokens,
	}
}

// TestResolve_YesNoBinary covers the canonical 2-outcome market
// path: outcomes=["Yes","No"], prices=["0.62","0.38"], tokens=[t1,t2].
func TestResolve_YesNoBinary(t *testing.T) {
	mk := mkMarket("ev", "0xa", "",
		[]string{"Yes", "No"},
		[]string{"0.62", "0.38"},
		[]string{"tokYes", "tokNo"},
	)
	m := NewMapper([]repository.EventPageMarketRow{mk})

	yes, ok := m.ResolveByConditionAndOutcome("0xa", "Yes")
	if !ok || !yes.IsYes || yes.OutcomeIndex != 0 || yes.CurrentPrice != 0.62 {
		t.Errorf("yes mapping wrong: %+v", yes)
	}
	if yes.CLOBTokenID != "tokYes" || yes.Reason != "exact_label" {
		t.Errorf("yes mapping details wrong: %+v", yes)
	}
	no, ok := m.ResolveByConditionAndOutcome("0xa", "No")
	if !ok || !no.IsNo || no.OutcomeIndex != 1 || no.CurrentPrice != 0.38 {
		t.Errorf("no mapping wrong: %+v", no)
	}
}

// TestResolve_MultiCandidateGroupItemTitle covers the texas-runoff
// pattern: each candidate is its own market with outcomes=["Yes","No"]
// and the candidate's name on GroupItemTitle.
func TestResolve_MultiCandidateGroupItemTitle(t *testing.T) {
	a := mkMarket("texas", "0xpaxton", "Ken Paxton",
		[]string{"Yes", "No"},
		[]string{"0.55", "0.45"},
		[]string{"toxYes", "toxNo"},
	)
	b := mkMarket("texas", "0xcornyn", "John Cornyn",
		[]string{"Yes", "No"},
		[]string{"0.40", "0.60"},
		[]string{"tocYes", "tocNo"},
	)
	m := NewMapper([]repository.EventPageMarketRow{a, b})

	// Annotation outcome "Ken Paxton" should map to Paxton's market.
	got, ok := m.ResolveByEventSlugAndAnnotationOutcome("texas", "Ken Paxton")
	if !ok {
		t.Fatalf("expected mapping; got %+v", got)
	}
	if got.ConditionID != "0xpaxton" || got.CurrentPrice != 0.55 {
		t.Errorf("paxton mapping wrong: %+v", got)
	}
	if got.Reason != "group_item_title" || got.Confidence < 0.6 {
		t.Errorf("paxton confidence/reason wrong: %+v", got)
	}
	got, ok = m.ResolveByEventSlugAndAnnotationOutcome("texas", "John Cornyn")
	if !ok || got.ConditionID != "0xcornyn" || got.CurrentPrice != 0.40 {
		t.Errorf("cornyn mapping wrong: %+v", got)
	}
}

// TestResolve_ByTokenID covers the trade-row path where the
// outcome_token is the load-bearing key.
func TestResolve_ByTokenID(t *testing.T) {
	mk := mkMarket("ev", "0xa", "",
		[]string{"Yes", "No"},
		[]string{"0.7", "0.3"},
		[]string{"tokYes", "tokNo"},
	)
	m := NewMapper([]repository.EventPageMarketRow{mk})

	got, ok := m.ResolveByTokenID("tokNo")
	if !ok || !got.IsNo || got.CurrentPrice != 0.3 || got.Reason != "token_match" {
		t.Errorf("no-token mapping wrong: %+v", got)
	}
	// Whitespace tolerance.
	got, ok = m.ResolveByTokenID("  tokYes  ")
	if !ok || !got.IsYes || got.CurrentPrice != 0.7 {
		t.Errorf("yes-token mapping wrong with whitespace: %+v", got)
	}
}

// TestResolve_UnknownReturnsExplicitReason pins the no-invention
// contract: misses produce a stable Reason code, never an invented
// mapping.
func TestResolve_UnknownReturnsExplicitReason(t *testing.T) {
	m := NewMapper(nil)

	got, ok := m.ResolveByConditionAndOutcome("0xnonexistent", "Yes")
	if ok || got.Reason != "unknown_condition_id" {
		t.Errorf("expected unknown_condition_id; got %+v ok=%v", got, ok)
	}
	got, ok = m.ResolveByTokenID("notreal")
	if ok || got.Reason != "unknown_token_id" {
		t.Errorf("expected unknown_token_id; got %+v ok=%v", got, ok)
	}
	got, ok = m.ResolveByEventSlugAndAnnotationOutcome("notreal", "Yes")
	if ok || got.Reason != "unknown_event_slug" {
		t.Errorf("expected unknown_event_slug; got %+v ok=%v", got, ok)
	}

	// Label that doesn't match anything in a known market.
	mk := mkMarket("ev", "0xa", "Ken Paxton",
		[]string{"Yes", "No"},
		[]string{"0.5", "0.5"},
		[]string{"t1", "t2"},
	)
	m = NewMapper([]repository.EventPageMarketRow{mk})
	got, ok = m.ResolveByConditionAndOutcome("0xa", "Some Other Name")
	if ok || got.Reason != "label_not_found" {
		t.Errorf("expected label_not_found; got %+v ok=%v", got, ok)
	}
}

// TestResolve_AnnotationTextMappingFold pins the case-insensitive
// fallback path used when Polymarket emits an annotation with
// "yes" lower-case vs the market's "Yes" capital.
func TestResolve_AnnotationTextMappingFold(t *testing.T) {
	mk := mkMarket("ev", "0xa", "",
		[]string{"Yes", "No"},
		[]string{"0.55", "0.45"},
		[]string{"t1", "t2"},
	)
	m := NewMapper([]repository.EventPageMarketRow{mk})

	got, ok := m.ResolveByConditionAndOutcome("0xa", "yes")
	if !ok || !got.IsYes || got.Reason != "case_insensitive_label" {
		t.Errorf("case-insensitive yes mapping wrong: %+v", got)
	}
	if got.Confidence < 0.5 || got.Confidence >= 1.0 {
		t.Errorf("case-insensitive confidence should be < 1.0; got %v", got.Confidence)
	}
}
