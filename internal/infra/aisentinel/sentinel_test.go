package aisentinel

import "testing"

// PART 17 tests 18-19: ParseAIContractResult parses sentinel; rejects
// invalid prose in JSON-only ranking surfaces.

func TestParse_SentinelExactMatch(t *testing.T) {
	cases := map[string]Code{
		"AI_NO_NOTICEABLE_EDGE":      CodeNoNoticeableEdge,
		"AI_ALREADY_PRICED":          CodeAlreadyPriced,
		"AI_CONTEXT_STALE":           CodeContextStale,
		"AI_INPUT_INSUFFICIENT":      CodeInputInsufficient,
		"AI_ONLY_RESOLUTION_BLOCKED": CodeOnlyResolutionBlocked,
	}
	p := New(false)
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			r := p.Parse(raw)
			if r.Kind != KindSentinel {
				t.Errorf("kind: got %v want sentinel", r.Kind)
			}
			if r.Code != want {
				t.Errorf("code: got %q want %q", r.Code, want)
			}
		})
	}
}

func TestParse_SentinelWithSurroundingWhitespace(t *testing.T) {
	p := New(false)
	r := p.Parse("\n   AI_NO_NOTICEABLE_EDGE   \n")
	if r.Kind != KindSentinel || r.Code != CodeNoNoticeableEdge {
		t.Errorf("expected sentinel passthrough; got %+v", r)
	}
}

func TestParse_JSONSelectionHappyPath(t *testing.T) {
	p := New(true)
	r := p.Parse(`{
		"regime": "news_changed",
		"should_request_full_analysis": true,
		"reason": "iran negotiations spike",
		"selected": [
			{"event_slug": "x", "condition_id": "0xa", "rank": 1, "interest_score": 0.85, "class": "informed_flow", "why_now": "p99 trade with news confirm", "expected_direction": "YES_up", "full_analysis_needed": true}
		]
	}`)
	if r.Kind != KindJSONSelection {
		t.Fatalf("expected JSON selection; got %v", r.Kind)
	}
	if len(r.JSON.Selected) != 1 || r.JSON.Selected[0].EventSlug != "x" {
		t.Errorf("payload not parsed: %+v", r.JSON)
	}
}

// Spec: empty `selected` array MUST collapse to a sentinel — the AI
// is instructed never to emit `selected=[]`.
func TestParse_EmptySelectedCollapsesToSentinel(t *testing.T) {
	p := New(true)
	r := p.Parse(`{"regime":"x","selected":[]}`)
	if r.Kind != KindSentinel || r.Code != CodeNoNoticeableEdge {
		t.Errorf("empty selected must become sentinel; got %+v", r)
	}
}

// JSON-only surface MUST reject free-text output.
func TestParse_JSONSurfaceRejectsProse(t *testing.T) {
	p := New(true)
	r := p.Parse("Iran flow is interesting because…")
	if r.Kind != KindInvalid {
		t.Errorf("expected invalid; got %v (%q)", r.Kind, r.Text)
	}
}

// Text-mode surface accepts prose.
func TestParse_TextSurfaceAcceptsProse(t *testing.T) {
	p := New(false)
	r := p.Parse("Prediction\n• Thesis: ...\n• Direction: YES_up\n")
	if r.Kind != KindPredictionText {
		t.Errorf("expected prediction_text; got %v", r.Kind)
	}
	if !contains(r.Text, "Thesis") {
		t.Errorf("text not preserved: %q", r.Text)
	}
}

func TestParse_EmptyIsInvalid(t *testing.T) {
	if New(false).Parse("").Kind != KindInvalid {
		t.Error("empty input must be invalid")
	}
	if New(true).Parse("   \n").Kind != KindInvalid {
		t.Error("whitespace-only must be invalid")
	}
}

// IsKnownSentinel + SuppressesTelegram are the small public helpers
// surfaces use directly.
func TestSentinelHelpers(t *testing.T) {
	if !IsKnownSentinel("AI_ALREADY_PRICED") {
		t.Error("must recognise AI_ALREADY_PRICED")
	}
	if IsKnownSentinel("AI_SOMETHING_ELSE") {
		t.Error("must reject unknown code")
	}
	for _, c := range []Code{CodeNoNoticeableEdge, CodeAlreadyPriced, CodeContextStale, CodeInputInsufficient, CodeOnlyResolutionBlocked} {
		if !c.SuppressesTelegram() {
			t.Errorf("%s must suppress Telegram by default", c)
		}
	}
}

// Output rules: the prompt appendix must mention every sentinel and
// the "EXACTLY one line" rule. v10.8 leads with the PascalCase
// AiAnswered* form (operator-preferred) while still acknowledging
// the legacy SCREAMING_SNAKE codes.
func TestPromptAppendixMentionsEverySentinel(t *testing.T) {
	v108 := []string{
		"AiAnsweredNotFoundNoticeable",
		"AiAnsweredAlreadyPriced",
		"AiAnsweredContextStale",
		"AiAnsweredInsufficientData",
		"AiAnsweredOnlyResolutionBlocked",
	}
	for _, name := range v108 {
		if !contains(PromptAppendix, name) {
			t.Errorf("appendix missing v10.8 sentinel %s", name)
		}
	}
	for _, code := range []Code{
		CodeNoNoticeableEdge, CodeAlreadyPriced, CodeContextStale,
		CodeInputInsufficient, CodeOnlyResolutionBlocked,
	} {
		if !contains(PromptAppendix, string(code)) {
			t.Errorf("appendix missing legacy %s", code)
		}
	}
	if !contains(PromptAppendix, "EXACTLY one line") {
		t.Error("appendix must state the exact-match rule")
	}
}

// PART 17 (v10.8): the v10.8 PascalCase aliases must parse to the
// canonical Code values.
func TestParse_v108PascalCaseAliases(t *testing.T) {
	cases := map[string]Code{
		"AiAnsweredNotFoundNoticeable":    CodeNoNoticeableEdge,
		"AiAnsweredAlreadyPriced":         CodeAlreadyPriced,
		"AiAnsweredContextStale":          CodeContextStale,
		"AiAnsweredInsufficientData":      CodeInputInsufficient,
		"AiAnsweredOnlyResolutionBlocked": CodeOnlyResolutionBlocked,
	}
	p := New(false)
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			r := p.Parse(raw)
			if r.Kind != KindSentinel {
				t.Errorf("kind: got %v want sentinel", r.Kind)
			}
			if r.Code != want {
				t.Errorf("code: got %q want %q", r.Code, want)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
