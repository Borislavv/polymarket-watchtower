// Package aipreflight is the per-surface AI call-budget gate.
//
// Every AI surface in the system (alert analyzer, catalyst importer,
// prediction creation ranker + thesis, prediction evolution thesis,
// market intel, daily political intel) MUST run its prompt through
// Preflight before issuing the HTTP call. The preflight:
//
//  1. Measures the prompt size against a per-surface char cap.
//  2. If over-cap, runs the configured Compactor to drop low-priority
//     sections, then re-measures.
//  3. If still over-cap, returns Decision{Skip: true, Reason:"chars_cap"}
//     — the caller short-circuits without an HTTP call.
//  4. Estimates pre-flight cost from the compacted char count + the
//     configured per-1k-input/output USD prices.
//  5. Asks the aibudget.Manager whether the surface's daily bucket
//     and the global budget have headroom for the estimated cost.
//
// On the happy path, returns Decision{Allow: true, Prompt: …,
// EstCostUSD: …, MaxOutputTokens: …}. The caller MUST then call
// Charge(actualCost) after the HTTP call lands.
//
// Everything in this package is deterministic + cheap (no I/O).
// nil Manager / nil metrics are tolerated (fail-open).
package aipreflight

import (
	"github.com/Borislavv/polymarket-watchtower/internal/infra/aibudget"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// SurfaceConfig holds per-surface caps + cost knobs. One instance
// per AI surface; passed by value to Preflight.Run.
type SurfaceConfig struct {
	// Surface name, e.g. "prediction_creation" — used as the
	// Prometheus label across the preflight metrics + the budget
	// bucket id.
	Surface string
	// BudgetBucket is the aibudget bucket constant (see
	// aibudget.BucketPredictionCreate etc). Empty disables the
	// budget check (compaction + chars cap still apply).
	BudgetBucket string
	// MaxInputChars is the post-compaction char cap. 0 disables.
	MaxInputChars int
	// MaxOutputTokens is the request's max_completion_tokens
	// (clamped down on the OpenAI request). 0 disables the clamp.
	MaxOutputTokens int
	// EstCostUSD is the pre-flight per-call cost estimate the
	// budget gate uses. Pair with Charge() after the call returns
	// so the running total tracks actual spend.
	EstCostUSD float64
}

// Decision is what Preflight.Run returns.
type Decision struct {
	// Allow=true ⇒ the caller proceeds with the HTTP call using
	// `Prompt` as the user message and `MaxOutputTokens` as the
	// output cap. Allow=false ⇒ skip the call and emit a structured
	// log line with `Reason`.
	Allow           bool
	Prompt          string
	MaxOutputTokens int
	EstCostUSD      float64
	// Reason is filled on Allow=false. Stable enum:
	// "chars_cap" | "budget_bucket_exhausted" | "budget_global_exhausted".
	Reason string
}

// Compactor produces a smaller prompt from the same context. The
// preflight calls Compact() at most once per Run — if the result is
// still over the char cap, the call is skipped.
//
// Implementations are domain-specific (each AI surface knows what
// to drop first); the preflight is agnostic.
type Compactor interface {
	Compact(prompt string) (compacted string, dropped string)
}

// Preflight is the cheap, shared gate. Construct once per surface,
// reuse for every call.
type Preflight struct {
	cfg    SurfaceConfig
	budget *aibudget.Manager
	met    *metrics.Metrics
}

// New constructs a Preflight. nil budget / nil metrics are
// tolerated (fail-open).
func New(cfg SurfaceConfig, budget *aibudget.Manager, met *metrics.Metrics) *Preflight {
	return &Preflight{cfg: cfg, budget: budget, met: met}
}

// Run evaluates a candidate prompt + returns the Decision. The
// caller hands in an optional Compactor — pass nil to disable
// compaction (the over-cap path then short-circuits immediately).
func (p *Preflight) Run(prompt string, compactor Compactor) Decision {
	out := Decision{
		Prompt:          prompt,
		MaxOutputTokens: p.cfg.MaxOutputTokens,
		EstCostUSD:      p.cfg.EstCostUSD,
	}
	// Chars cap (post-compaction).
	if p.cfg.MaxInputChars > 0 && len(out.Prompt) > p.cfg.MaxInputChars && compactor != nil {
		compacted, _ := compactor.Compact(out.Prompt)
		out.Prompt = compacted
		p.observeCompaction("chars_cap")
	}
	if p.cfg.MaxInputChars > 0 && len(out.Prompt) > p.cfg.MaxInputChars {
		out.Reason = "chars_cap"
		p.observeSkipped("chars_cap")
		return out
	}
	p.observePromptChars(len(out.Prompt))

	// Budget gate.
	if p.budget != nil && p.cfg.BudgetBucket != "" {
		if ok, reason := p.budget.Allow(p.cfg.BudgetBucket, p.cfg.EstCostUSD); !ok {
			out.Reason = "budget_" + reason
			p.observeSkipped(out.Reason)
			return out
		}
	}
	p.observeEstimatedCost(p.cfg.EstCostUSD)
	out.Allow = true
	return out
}

// Charge records the actual cost on the surface's budget bucket.
// Always call this after a successful AI call lands (use the
// EstimatedCostUSD field on the AI response). Calling Charge after
// Allow=false is a contract violation.
func (p *Preflight) Charge(actualUSD float64) {
	if p.budget == nil || p.cfg.BudgetBucket == "" {
		return
	}
	p.budget.Charge(p.cfg.BudgetBucket, actualUSD)
}

// --- metric helpers (nil-safe) -------------------------------------------

func (p *Preflight) observeCompaction(reason string) {
	if p.met == nil || p.met.AICompactions == nil {
		return
	}
	p.met.AICompactions.WithLabelValues(p.cfg.Surface, reason).Inc()
}

func (p *Preflight) observePromptChars(n int) {
	if p.met == nil || p.met.AIPromptChars == nil {
		return
	}
	p.met.AIPromptChars.WithLabelValues(p.cfg.Surface).Observe(float64(n))
}

func (p *Preflight) observeSkipped(reason string) {
	if p.met == nil || p.met.AISurfaceSkipped == nil {
		return
	}
	p.met.AISurfaceSkipped.WithLabelValues(p.cfg.Surface, reason).Inc()
}

func (p *Preflight) observeEstimatedCost(usd float64) {
	if p.met == nil || p.met.AISurfaceEstimatedCost == nil || usd <= 0 {
		return
	}
	p.met.AISurfaceEstimatedCost.WithLabelValues(p.cfg.Surface).Add(usd)
}

// SimpleCompactor is a small drop-in compactor for callers that
// already pre-build the prompt with explicit section markers. It
// truncates the prompt to `cap` chars and appends a stable suffix
// so the model knows the prompt was abridged.
//
// Surface-specific Compactors that understand section priorities
// (drop old annotations first, drop sources second, etc.) should
// implement their own; this is the lowest-common-denominator
// fallback.
type SimpleCompactor struct {
	Cap int
}

func (s SimpleCompactor) Compact(prompt string) (string, string) {
	if s.Cap <= 0 || len(prompt) <= s.Cap {
		return prompt, ""
	}
	const tail = "\n\n[…context truncated for budget; lower-priority sections dropped…]"
	cut := s.Cap - len(tail)
	if cut < 1 {
		cut = 1
	}
	if cut >= len(prompt) {
		return prompt, ""
	}
	return prompt[:cut] + tail, prompt[cut:]
}
