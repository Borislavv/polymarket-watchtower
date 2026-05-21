// Package aisentinel is the v10.7 machine-readable AI-output contract.
//
// AI surfaces (marketintel batch ranking, prediction analysis, daily
// political intel) must return EITHER analytical output OR exactly
// one of the sentinel codes below. The codes are NOT errors from the
// provider — they are semantic NotFound / NoAction signals. The
// orchestrator routes on them:
//
//   - persist the sentinel result for audit (so we know the AI was
//     called and what it concluded);
//   - SUPPRESS Telegram delivery by default;
//   - increment per-surface metrics.
//
// This lets the AI honestly say "nothing interesting here" without
// the surface either inventing filler or silently dropping the call.
// It also gives the operator a programmable interface — downstream
// code routes on the Sentinel value, not on free-text prose.
package aisentinel

import (
	"encoding/json"
	"strings"
)

// Code is the stable string set of sentinel values. Output text from
// the model MUST be exactly one of these (no surrounding prose, no
// trailing newline beyond a single \n).
type Code string

const (
	// CodeNoNoticeableEdge — AI found nothing actionable.
	CodeNoNoticeableEdge Code = "AI_NO_NOTICEABLE_EDGE"

	// CodeAlreadyPriced — the move is already in the price; no edge.
	CodeAlreadyPriced Code = "AI_ALREADY_PRICED"

	// CodeContextStale — the eventpage/news context the AI was given
	// is stale and the model refuses to honestly analyze.
	CodeContextStale Code = "AI_CONTEXT_STALE"

	// CodeInputInsufficient — input lacks market/news/price/flow
	// data the prompt requires.
	CodeInputInsufficient Code = "AI_INPUT_INSUFFICIENT"

	// CodeOnlyResolutionBlocked — only "wait for election/resolution
	// day" intelligence was found; no pre-event edge. Specific to
	// prediction analysis paths.
	CodeOnlyResolutionBlocked Code = "AI_ONLY_RESOLUTION_BLOCKED"

	// v10.8 PascalCase aliases. The operator spec asked for these
	// names (AiAnsweredNotFoundNoticeable etc.) — they are accepted
	// AS ALTERNATIVE wire strings by the parser. The canonical
	// internal Code values remain the SCREAMING_SNAKE forms above so
	// existing tests, dashboards, and metrics labels don't churn.
	codeAliasNoNoticeableEdge      Code = "AiAnsweredNotFoundNoticeable"
	codeAliasAlreadyPriced         Code = "AiAnsweredAlreadyPriced"
	codeAliasContextStale          Code = "AiAnsweredContextStale"
	codeAliasInputInsufficient     Code = "AiAnsweredInsufficientData"
	codeAliasOnlyResolutionBlocked Code = "AiAnsweredOnlyResolutionBlocked"
)

// allCodes is the recognised set. Used by Parse() to detect a sentinel.
// Includes both canonical SCREAMING_SNAKE and v10.8 PascalCase aliases —
// the parser normalises every alias back to the canonical Code.
var allCodes = []Code{
	CodeNoNoticeableEdge,
	CodeAlreadyPriced,
	CodeContextStale,
	CodeInputInsufficient,
	CodeOnlyResolutionBlocked,
	codeAliasNoNoticeableEdge,
	codeAliasAlreadyPriced,
	codeAliasContextStale,
	codeAliasInputInsufficient,
	codeAliasOnlyResolutionBlocked,
}

// canonicalCode maps both wire forms (SCREAMING_SNAKE and v10.8
// PascalCase) onto the canonical Code value the rest of the codebase
// uses. Lookup is case-insensitive for the SCREAMING_SNAKE codes;
// PascalCase aliases match case-sensitively per the spec.
var canonicalCode = map[string]Code{
	"AI_NO_NOTICEABLE_EDGE":           CodeNoNoticeableEdge,
	"AI_ALREADY_PRICED":               CodeAlreadyPriced,
	"AI_CONTEXT_STALE":                CodeContextStale,
	"AI_INPUT_INSUFFICIENT":           CodeInputInsufficient,
	"AI_ONLY_RESOLUTION_BLOCKED":      CodeOnlyResolutionBlocked,
	"AiAnsweredNotFoundNoticeable":    CodeNoNoticeableEdge,
	"AiAnsweredAlreadyPriced":         CodeAlreadyPriced,
	"AiAnsweredContextStale":          CodeContextStale,
	"AiAnsweredInsufficientData":      CodeInputInsufficient,
	"AiAnsweredOnlyResolutionBlocked": CodeOnlyResolutionBlocked,
}

// Kind enumerates the result classes the parser returns.
type Kind int

const (
	// KindInvalid — neither sentinel nor recognised analytical output.
	KindInvalid Kind = iota
	// KindSentinel — exact-match sentinel code.
	KindSentinel
	// KindJSONSelection — JSON-shape output for the batch ranking path.
	KindJSONSelection
	// KindPredictionText — free-text analytical prose for the
	// prediction-analysis / marketintel-summary path.
	KindPredictionText
)

func (k Kind) String() string {
	switch k {
	case KindSentinel:
		return "sentinel"
	case KindJSONSelection:
		return "json_selection"
	case KindPredictionText:
		return "prediction_text"
	default:
		return "invalid"
	}
}

// Result is the typed parser output.
type Result struct {
	Kind Kind
	// Code is populated when Kind=KindSentinel.
	Code Code
	// JSON is the parsed JSON-selection payload when Kind=KindJSONSelection.
	JSON *JSONSelection
	// Text is the cleaned analytical body when Kind=KindPredictionText.
	Text string
	// RawPreview is the first 300 chars of the raw model output, for
	// the structured request-log row.
	RawPreview string
}

// JSONSelection is the strict shape the PART 9 batch-ranking prompt
// promises. Extra fields are tolerated (we read what we need).
type JSONSelection struct {
	Regime                    string               `json:"regime"`
	ShouldRequestFullAnalysis bool                 `json:"should_request_full_analysis"`
	Reason                    string               `json:"reason"`
	Selected                  []JSONSelectionEntry `json:"selected"`
}

// JSONSelectionEntry is one ranked market row.
type JSONSelectionEntry struct {
	EventSlug          string  `json:"event_slug"`
	ConditionID        string  `json:"condition_id"`
	Rank               int     `json:"rank"`
	InterestScore      float64 `json:"interest_score"`
	Class              string  `json:"class"`
	WhyNow             string  `json:"why_now"`
	ExpectedDirection  string  `json:"expected_direction"`
	FullAnalysisNeeded bool    `json:"full_analysis_needed"`

	// v10.8 evaluator fields. The evaluator prompt asks the AI to
	// produce a 2-3 sentence thesis + invalidation + watch-next so
	// the Telegram surface has dense actionable content. Older
	// prompts (v10.7 PART 9 batch ranking) omit these — they parse
	// as empty strings.
	Thesis              string `json:"thesis"`
	WhatWouldInvalidate string `json:"what_would_invalidate"`
	WhatToWatchNext     string `json:"what_to_watch_next"`
}

// Parser configuration. Construct one per AI surface — the
// `expectsJSON` flag flips the parser between "JSON-selection or
// sentinel only" (batch-ranking surfaces) and "free-text prediction
// or sentinel only" (single-market analysis surfaces).
type Parser struct {
	expectsJSON bool
}

// New returns a Parser. When expectsJSON=true, free-text output is
// rejected as InvalidFormat (the model violated the contract). When
// expectsJSON=false, free-text is accepted as KindPredictionText.
func New(expectsJSON bool) *Parser {
	return &Parser{expectsJSON: expectsJSON}
}

// Parse routes raw model output into a typed Result. The decision tree:
//
//  1. Trim whitespace. If empty → KindInvalid.
//  2. If first non-space line is exactly a recognised sentinel code
//     (and the rest of the output is empty / whitespace) →
//     KindSentinel{Code}.
//  3. If the body starts with `{` or `[` and the parser is JSON-mode →
//     attempt JSON decode. Empty `selected` is treated as
//     KindSentinel{NoNoticeableEdge} per the prompt rule.
//  4. If parser is text-mode → return KindPredictionText.
//  5. Otherwise → KindInvalid.
//
// Parse NEVER errors — it always returns a typed Result and never
// panics on malformed input. Callers decide what to do with
// KindInvalid (the typical action is "log + skip + do not store as
// analysis").
func (p *Parser) Parse(raw string) Result {
	trimmed := strings.TrimSpace(raw)
	out := Result{RawPreview: previewOf(trimmed, 300)}
	if trimmed == "" {
		out.Kind = KindInvalid
		return out
	}
	if code, ok := detectSentinel(trimmed); ok {
		out.Kind = KindSentinel
		out.Code = code
		return out
	}
	first := trimmed[0]
	if p.expectsJSON {
		if first != '{' && first != '[' {
			out.Kind = KindInvalid
			return out
		}
		var sel JSONSelection
		if err := json.Unmarshal([]byte(trimmed), &sel); err != nil {
			out.Kind = KindInvalid
			return out
		}
		if len(sel.Selected) == 0 {
			// Spec: "Do not return JSON with selected=[]; use
			// AI_NO_NOTICEABLE_EDGE instead." Treat as sentinel so
			// the orchestrator routes identically.
			out.Kind = KindSentinel
			out.Code = CodeNoNoticeableEdge
			return out
		}
		out.Kind = KindJSONSelection
		out.JSON = &sel
		return out
	}
	// Text mode: accept the prose, but if it looks like JSON-only the
	// surface contract was violated.
	if first == '{' || first == '[' {
		out.Kind = KindInvalid
		return out
	}
	out.Kind = KindPredictionText
	out.Text = trimmed
	return out
}

// detectSentinel returns the sentinel Code when the entire trimmed
// output is exactly one of the recognised codes. SCREAMING_SNAKE
// codes match case-insensitively; PascalCase v10.8 aliases match
// case-sensitively per the operator spec. Every alias is normalised
// to the canonical Code before return.
func detectSentinel(trimmed string) (Code, bool) {
	if code, ok := canonicalCode[trimmed]; ok {
		return code, true
	}
	upper := strings.ToUpper(trimmed)
	if code, ok := canonicalCode[upper]; ok {
		return code, true
	}
	return "", false
}

func previewOf(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// SuppressesTelegram reports whether a sentinel code MUST suppress
// Telegram delivery by default. Every code does today; kept as a
// helper so the operator can flip individual codes via config later
// without changing call sites.
func (c Code) SuppressesTelegram() bool {
	switch c {
	case CodeNoNoticeableEdge, CodeAlreadyPriced, CodeContextStale,
		CodeInputInsufficient, CodeOnlyResolutionBlocked:
		return true
	}
	return false
}

// IsKnownSentinel returns true when `s` matches any of the
// recognised codes. Used by surfaces that consume sentinels without
// going through the full Parser.
func IsKnownSentinel(s string) bool {
	_, ok := detectSentinel(strings.TrimSpace(s))
	return ok
}

// PromptAppendix returns the canonical sentinel-contract block to
// append to AI prompts that consume the v10.7 contract. The block
// instructs the model on the exact-match rule and lists every
// recognised code. Tests pin this string so a prompt change can't
// silently drift.
const PromptAppendix = `If nothing is interesting / no actionable edge: return EXACTLY one line:
AiAnsweredNotFoundNoticeable

If the market move is already priced: return EXACTLY one line:
AiAnsweredAlreadyPriced

If context is stale / news did not change: return EXACTLY one line:
AiAnsweredContextStale

If only "blocked until resolution day" intelligence was found: return EXACTLY one line:
AiAnsweredOnlyResolutionBlocked

If the input lacks market / news / price / flow data: return EXACTLY one line:
AiAnsweredInsufficientData

Rules:
- One sentinel line means: no other text, no prose, no JSON.
- Sentinels are NOT errors; they are valid programmatic results.
- Otherwise return the analytical output the prompt requests.

(Legacy SCREAMING_SNAKE forms — AI_NO_NOTICEABLE_EDGE, AI_ALREADY_PRICED,
AI_CONTEXT_STALE, AI_ONLY_RESOLUTION_BLOCKED, AI_INPUT_INSUFFICIENT — are
also recognised. Either family is acceptable; pick one and use it
consistently.)`
