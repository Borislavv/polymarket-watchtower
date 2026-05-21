// PART 9 / PART 21 + PART 8: pin the v11.0 news-intel JSON parser
// and sentinel detection so a future edit can't silently weaken them.
package openai

import (
	"strings"
	"testing"
)

func TestDetectV11Sentinel_AllPascalCaseCodes(t *testing.T) {
	for _, code := range []string{
		"AiAnsweredNotFoundNoticeable",
		"AiAnsweredAlreadyPriced",
		"AiAnsweredContextStale",
		"AiAnsweredInsufficientData",
		"AiAnsweredLowConfidenceSkip",
	} {
		got, ok := detectV11Sentinel(code)
		if !ok || got != code {
			t.Errorf("detectV11Sentinel(%q) = (%q, %v); want (%q, true)", code, got, ok, code)
		}
	}
}

func TestDetectV11Sentinel_RejectsNoise(t *testing.T) {
	for _, in := range []string{
		"", "AiAnsweredOther", "{\"decision\":\"watch\"}", "AiAnswered",
	} {
		if got, ok := detectV11Sentinel(in); ok {
			t.Errorf("detectV11Sentinel(%q) unexpectedly matched -> %q", in, got)
		}
	}
}

func TestParseNewsIntelJSON_FullShape(t *testing.T) {
	body := `{
		"decision": "actionable",
		"summary": "endorsement creates fresh window",
		"selected": [
			{
				"news_item_hash": "abc",
				"event_slug": "election-2026",
				"condition_id": "0xCOND",
				"market_title": "Will candidate win?",
				"rank": 1,
				"confidence": 0.82,
				"impact_direction": "YES_up",
				"expected_window": "12h",
				"why_it_matters": "endorser swings the runoff",
				"trade_stance": "consider",
				"telegram_worthy": true
			}
		]
	}`
	out, err := ParseNewsIntelJSON(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Decision != "actionable" {
		t.Fatalf("decision: %q", out.Decision)
	}
	if len(out.Selected) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(out.Selected))
	}
	d := out.Selected[0]
	if d.ImpactDirection != "YES_up" {
		t.Errorf("impact_direction normalised wrong: %q", d.ImpactDirection)
	}
	if d.ExpectedWindow != "12h" {
		t.Errorf("expected_window normalised wrong: %q", d.ExpectedWindow)
	}
	if !d.TelegramWorthy {
		t.Errorf("telegram_worthy lost")
	}
}

func TestParseNewsIntelJSON_BadEnumsCoerced(t *testing.T) {
	body := `{
		"decision": "garbage",
		"summary": "x",
		"selected": [
			{
				"news_item_hash": "abc",
				"event_slug": "election-2026",
				"condition_id": "0xCOND",
				"impact_direction": "DEFINITELY_YES",
				"expected_window": "soon-ish",
				"trade_stance": "BUY EVERYTHING",
				"confidence": 1.5
			}
		]
	}`
	out, err := ParseNewsIntelJSON(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Decision != "watch" {
		t.Errorf("bad decision should coerce to watch, got %q", out.Decision)
	}
	if out.Selected[0].ImpactDirection != "unclear" {
		t.Errorf("bad impact direction should coerce to unclear, got %q", out.Selected[0].ImpactDirection)
	}
	if out.Selected[0].ExpectedWindow != "unclear" {
		t.Errorf("bad expected window should coerce to unclear")
	}
	if out.Selected[0].TradeStance != "watch" {
		t.Errorf("bad trade_stance should coerce to watch")
	}
	if out.Selected[0].Confidence != 1.0 {
		t.Errorf("confidence > 1 should clamp to 1, got %v", out.Selected[0].Confidence)
	}
}

func TestParseNewsIntelJSON_RejectsMarkdownWrap(t *testing.T) {
	_, err := ParseNewsIntelJSON("```json\n{\"decision\":\"watch\"}\n```")
	if err == nil {
		t.Fatalf("markdown-wrapped output must be rejected")
	}
}

func TestParseNewsIntelJSON_DropsEmptyRows(t *testing.T) {
	body := `{
		"decision": "watch",
		"selected": [
			{"news_item_hash": "", "event_slug": "x", "condition_id": "y"},
			{"news_item_hash": "ok", "event_slug": "x", "condition_id": "y"}
		]
	}`
	out, err := ParseNewsIntelJSON(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Selected) != 1 {
		t.Fatalf("expected 1 selected after filtering empty hash, got %d", len(out.Selected))
	}
	if out.Selected[0].NewsItemHash != "ok" {
		t.Errorf("wrong row survived: %+v", out.Selected[0])
	}
}

func TestBuildHourlyNewsIntelUserMessage_SubstitutesMaxSelected(t *testing.T) {
	req := NewsIntelAIRequest{MaxSelected: 5}
	msg := buildHourlyNewsIntelUserMessage(req)
	if !strings.Contains(msg, "Select at most 5 items.") {
		t.Fatalf("{{MAX_SELECTED}} not substituted: %q", msg[:min(300, len(msg))])
	}
	if strings.Contains(msg, "{{MAX_SELECTED}}") {
		t.Fatalf("placeholder leaked into rendered prompt")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
