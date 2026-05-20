package openai

import (
	"strings"
	"testing"
)

func TestParseAnnotationRankingJSON_AcceptsValidPayload(t *testing.T) {
	body := `{
        "selected": [
            {
                "event_slug": "texas",
                "market_slug": "paxton",
                "rank": 1,
                "importance": 0.9,
                "volatility_potential": 0.7,
                "probability_impact": "bullish",
                "affected_outcome": "Ken Paxton",
                "title": "Paxton up 63% in poll",
                "reason": "final pre-runoff poll lifts probability",
                "market_read": "underreacting"
            }
        ]
    }`
	got, err := ParseAnnotationRankingJSON(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Selected) != 1 {
		t.Fatalf("selected: got %d want 1", len(got.Selected))
	}
	s := got.Selected[0]
	if s.EventSlug != "texas" || s.Title != "Paxton up 63% in poll" {
		t.Errorf("decoded wrong: %+v", s)
	}
	if s.ProbabilityImpact != "bullish" || s.MarketRead != "underreacting" {
		t.Errorf("enums lost: %+v", s)
	}
}

func TestParseAnnotationRankingJSON_RejectsMarkdownWrap(t *testing.T) {
	if _, err := ParseAnnotationRankingJSON("```json\n{\"selected\":[]}\n```"); err == nil {
		t.Fatal("expected error rejecting markdown wrap")
	}
}

func TestParseAnnotationRankingJSON_NormalisesBadEnums(t *testing.T) {
	body := `{"selected":[
        {"event_slug":"x","title":"a","probability_impact":"wrong","market_read":"also_wrong","importance":2,"volatility_potential":-0.5,"rank":1}
    ]}`
	got, _ := ParseAnnotationRankingJSON(body)
	if len(got.Selected) != 1 {
		t.Fatalf("expected 1 selected (bad enums normalise, not drop)")
	}
	s := got.Selected[0]
	if s.ProbabilityImpact != "unclear" {
		t.Errorf("bad probability_impact must collapse to unclear: %q", s.ProbabilityImpact)
	}
	if s.MarketRead != "unclear" {
		t.Errorf("bad market_read must collapse to unclear: %q", s.MarketRead)
	}
	if s.Importance != 1 || s.VolatilityPotential != 0 {
		t.Errorf("scores not clamped: %+v", s)
	}
}

func TestParseAnnotationRankingJSON_DropsRowsWithEmptyTitleOrSlug(t *testing.T) {
	body := `{"selected":[
        {"event_slug":"","title":"a","probability_impact":"bullish","market_read":"watch","rank":1},
        {"event_slug":"x","title":"","probability_impact":"bullish","market_read":"watch","rank":2},
        {"event_slug":"x","title":"ok","probability_impact":"bullish","market_read":"watch","rank":3}
    ]}`
	got, _ := ParseAnnotationRankingJSON(body)
	if len(got.Selected) != 1 || got.Selected[0].Title != "ok" {
		t.Errorf("filter wrong: %+v", got.Selected)
	}
}

func TestBuildAnnotationRankingUserMessage_PlaceholdersSubstituted(t *testing.T) {
	// Build a minimal request and confirm every placeholder is
	// replaced (no `{{...}}` token leaks to the model).
	msg := buildAnnotationRankingUserMessage(stubRankingRequest())
	for _, ph := range []string{
		"{{OUTPUT_LIMIT}}", "{{PERIOD}}", "{{MARKETS}}",
		"{{ANNOTATIONS}}", "{{FLOW_SUMMARY}}",
	} {
		if strings.Contains(msg, ph) {
			t.Errorf("placeholder %s leaked through: %s", ph, msg)
		}
	}
	if !strings.Contains(msg, "Select at most 10 events") {
		t.Errorf("OUTPUT_LIMIT not substituted with 10")
	}
	if !strings.Contains(msg, "political/geopolitical prediction-market intelligence analyst") {
		t.Errorf("verbatim prompt must remain in user message")
	}
}
