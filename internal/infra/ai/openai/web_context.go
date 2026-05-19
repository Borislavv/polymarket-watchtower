// Web context (OpenAI Responses API + web_search tool) scaffold.
//
// STATUS: scaffolded but disabled by default. The HTTP contract
// against /v1/responses with the `web_search` tool needs verification
// against a live OpenAI account before production enable. Until
// then, ShouldUseWebContext returns false unless explicitly forced,
// and the client falls back to the existing chat-completions path
// (which surfaces PublicContextEnabled=false in the prompt so the
// model knows it has no live context and includes the canonical
// "Live context was not checked." disclosure).
//
// Why scaffold and not finish: the Responses API uses a different
// JSON shape from /chat/completions:
//
//	POST /v1/responses
//	{
//	  "model": "gpt-4o-mini",
//	  "input": [...],
//	  "tools": [{"type": "web_search_preview"}],
//	  ...
//	}
//
//	response: { "output": [ { "type": "message", "content": [
//	  { "type": "output_text", "text": "..." } ] } ], "usage": ... }
//
// The exact field names and the streaming/citations format are
// stable but I cannot exercise them against a live API from here.
// Wiring blind would risk shipping a broken AI path; the safe move
// is: gate behind a feature flag, scaffold the request/response
// shapes, and require operator verification before flipping
// `AI_ANALYSIS_WEB_CONTEXT_ENABLED=true` in production.
package openai

import (
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
)

// WebContextPolicy decides whether a given alert qualifies for the
// web_search-enabled prompt. The decision is operator-tunable via
// env knobs; defaults mirror the spec (Warning/Critical/Hard get
// web; HOT-Info and stable_favorite always get web when global
// toggle is on).
type WebContextPolicy struct {
	Enabled           bool
	MinSeverity       string // "info" | "warning" | "critical" | "hard"
	ForHotInfo        bool
	ForStableFavorite bool
	ForPolitics       bool
}

// ShouldUseWebContext returns true when the alert qualifies for the
// Responses-API + web_search path. The caller (aianalysis.Service)
// decides per-request which prompt + transport to use.
func (p WebContextPolicy) ShouldUseWebContext(req analysis.AlertAnalysisRequest, hotLifecycle bool) bool {
	if !p.Enabled {
		return false
	}
	if p.ForStableFavorite && req.Kind == "stable_favorite" {
		return true
	}
	if p.ForHotInfo && hotLifecycle {
		return true
	}
	if p.ForPolitics && strings.EqualFold(req.Category, "Politics") {
		return true
	}
	return severityAtLeast(req.Severity, p.MinSeverity)
}

// severityAtLeast returns true when `got` is at least as severe as
// `min`. Order: info < warning < critical < hard. Unknown severities
// rank as info so they fall under any non-empty floor.
func severityAtLeast(got, min string) bool {
	rank := func(s string) int {
		switch strings.ToLower(s) {
		case "hard":
			return 4
		case "critical":
			return 3
		case "warning":
			return 2
		default:
			return 1
		}
	}
	return rank(got) >= rank(min)
}
