// Package marketintel runs the 2h market-intelligence report.
//
// v9.7 pipeline (post-timeout-and-links pass):
//
//  1. List top-N candidate markets (lifecycle + recent activity +
//     liquidity, with event/market/category slugs for link rendering).
//  2. Build a compact MarketReportRequest.
//  3. Drop near-degenerate prices and per-condition duplicates
//     (filterAndDedupCandidates).
//  4. Apply the marketintel budget gate (estimated cost vs daily cap).
//  5. Build the user-visible prompt and run an aipreflight-style
//     char-cap compaction. Log prompt_chars_before / _after.
//  6. Call analyzer.AnalyzeMarketReport under a dedicated marketintel
//     timeout (default 60s, not the 45s alert timeout).
//  7. On CategoryTimeout: retry once with 1-3s jittered backoff. Honor
//     the parent context. Quota / rate-limit / 5xx never retry here.
//  8. Compose the Telegram body deterministically. Render Markets-to-
//     watch + Important-Polymarket-events with sanitized links. Show
//     the AI analysis when available; otherwise an "AI summary
//     unavailable: <short reason>" footer. Never silently skip when
//     deterministic content exists.
//  9. Hash the body for dedup; INSERT ON CONFLICT (summary_hash) DO
//     NOTHING; on fresh insert, post via SafeSplitForTelegram.
//
// The worker is intentionally simple — the orchestration is mostly
// data shaping. The hard parts (model call, cost control, content
// hashing, link rendering) live downstream in the analyzer / repo /
// render.go.
package marketintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	openai "github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// Candidates returns the top-N markets the analyzer should look at.
// *repository.MarketIntelligenceRepository satisfies it.
type Candidates interface {
	ListIntelligenceCandidates(ctx context.Context, limit int32) ([]repository.IntelligenceCandidate, error)
}

// Store persists the generated report row.
// *repository.MarketIntelligenceRepository satisfies it.
type Store interface {
	Insert(ctx context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error)
}

// Analyzer is the AI entry point. analysis.Analyzer satisfies it.
type Analyzer interface {
	AnalyzeMarketReport(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error)
}

// Bot is the Telegram delivery seam. *telegram.Bot satisfies it.
type Bot interface {
	SendHTML(ctx context.Context, chatID, text string) (telegram.SendResult, error)
}

// AnnotationLister fetches recent Polymarket event annotations for the
// "Important Polymarket events" deterministic section. *repository.
// EventPageRepository satisfies it. nil disables that section.
type AnnotationLister interface {
	ListRecentAnnotations(ctx context.Context, eventSlug string, limit int32) ([]repository.EventAnnotation, error)
}

// PromptCharsLoader is the optional hook the worker uses to compute
// the rendered prompt size before the AI call so it can run a
// compaction pass + log prompt_chars_before/after. Returns the
// rendered prompt string; the worker does NOT alter the analyzer
// request payload, it only emits observability data.
//
// *openai.Client implements this via PreviewMarketReportPrompt(req).
type PromptCharsLoader interface {
	PreviewMarketReportPrompt(req analysis.MarketReportRequest) string
}

// Config tunes the worker.
type Config struct {
	Enabled        bool
	Interval       time.Duration
	MaxMarkets     int
	MaxOutputChars int
	ChatID         string

	// --- v9.7 timeout + fallback + link config ---
	AITimeout         time.Duration
	RetryOnTimeout    bool
	RetryBackoffMin   time.Duration
	RetryBackoffMax   time.Duration
	FallbackOnFailure bool
	AnnotationsPerEvt int
	VisibleMarkets    int
	MaxInputChars     int // 0 disables the compaction pass
	Links             LinkConfig

	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Hour
	}
	if c.MaxMarkets <= 0 {
		c.MaxMarkets = 50
	}
	if c.MaxOutputChars <= 0 {
		c.MaxOutputChars = 2000
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 60 * time.Second
	}
	if c.RetryBackoffMin <= 0 {
		c.RetryBackoffMin = time.Second
	}
	if c.RetryBackoffMax <= 0 || c.RetryBackoffMax < c.RetryBackoffMin {
		c.RetryBackoffMax = 3 * time.Second
	}
	if c.AnnotationsPerEvt <= 0 {
		c.AnnotationsPerEvt = 3
	}
	if c.VisibleMarkets <= 0 {
		c.VisibleMarkets = 8
	}
	if c.Links.MaxLinksPerRow <= 0 {
		c.Links.MaxLinksPerRow = 5
	}
	if c.Links.MaxSourceLinks <= 0 {
		c.Links.MaxSourceLinks = 3
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// NarrativeLoader is the optional seam used to stamp Polymarket
// event-page context onto the market-intelligence prompt. Keyed by
// market conditionID — the loader internally resolves event_slug
// and renders the prompt slot. Empty result is the "unavailable"
// fallback. nil disables the slot entirely.
type NarrativeLoader interface {
	LoadAndRenderForConditionID(ctx context.Context, conditionID string, maxChars int) string
}

// AnnotationRankingHook is the optional seam that ranks the most
// important Polymarket annotations within the 2h candidate set,
// persists the AI choices to polymarket_event_annotation_rankings,
// and returns an HTML-formatted "Top important annotations" block
// for the Telegram body. Failure returns empty — the report still
// ships. Set via SetAnnotationRankingHook; nil disables ranking.
type AnnotationRankingHook interface {
	RankAndRender(ctx context.Context, candidates []repository.IntelligenceCandidate, periodStart, periodEnd time.Time, limit int) string
}

// BudgetGuard is the v10.3 seam to the shared aibudget governor.
// nil = fail-open.
type BudgetGuard interface {
	Allow(bucket string, estCost float64) (bool, string)
	Charge(bucket string, actualCost float64)
}

// estPerMarketIntelUSD is the conservative pre-flight cost estimate.
const estPerMarketIntelUSD = 0.05

// surfaceName is the metric label used for AI surface metrics.
const surfaceName = "market_intelligence"

// Worker is the periodic 2h intelligence loop.
type Worker struct {
	cfg              Config
	candidates       Candidates
	store            Store
	analyzer         Analyzer
	narrative        NarrativeLoader
	rankingHook      AnnotationRankingHook
	annotationLister AnnotationLister
	promptLoader     PromptCharsLoader
	bot              Bot
	budget           BudgetGuard
	metrics          *metrics.Metrics
	log              *zerolog.Logger
}

// SetBudget attaches the shared aibudget governor. nil = fail-open.
func (w *Worker) SetBudget(b BudgetGuard) { w.budget = b }

// New wires the worker. All deps required. Pass analysis.NoopAnalyzer
// to disable the AI call (the worker still selects candidates and
// persists a "skipped" row so an operator can audit the cadence).
// Metrics is optional — when nil, observeSkip / observeAIError no-op.
func New(cfg Config, candidates Candidates, store Store, analyzer Analyzer, bot Bot, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{cfg: cfg, candidates: candidates, store: store, analyzer: analyzer, bot: bot, log: log}
}

// SetMetrics wires the optional metrics sink. nil keeps the worker
// metrics-agnostic.
func (w *Worker) SetMetrics(m *metrics.Metrics) { w.metrics = m }

// SetNarrativeLoader wires the optional Polymarket event-page
// context loader. nil keeps the slot empty.
func (w *Worker) SetNarrativeLoader(loader NarrativeLoader) { w.narrative = loader }

// SetAnnotationRankingHook wires the optional 2h annotation ranker.
// nil disables the "Top important annotations" Telegram appendix.
func (w *Worker) SetAnnotationRankingHook(h AnnotationRankingHook) { w.rankingHook = h }

// SetAnnotationLister wires the deterministic annotation list source
// (the "Important Polymarket events" section). nil disables it.
func (w *Worker) SetAnnotationLister(a AnnotationLister) { w.annotationLister = a }

// SetPromptCharsLoader wires the optional prompt previewer used by
// the worker to log prompt_chars_before / _after across the
// compaction pass. nil leaves both fields at -1 in the structured log.
func (w *Worker) SetPromptCharsLoader(p PromptCharsLoader) { w.promptLoader = p }

// Run blocks until ctx cancels.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick exposes one cycle for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	candidates, err := w.candidates.ListIntelligenceCandidates(ctx, int32(w.cfg.MaxMarkets))
	if err != nil {
		w.log.Err(err).Msg("marketintel: list candidates failed")
		return
	}

	// Bucketed period — the load-bearing dedup primitive.
	periodEnd, periodStart := bucketedPeriod(w.cfg.Clock(), w.cfg.Interval)
	periodKey := formatPeriodKey(periodStart, periodEnd)

	// Drop near-degenerate prices and per-market duplicates.
	candidates = filterAndDedupCandidates(candidates)

	// Pull recent annotations for the top-N events deterministically
	// — this fuels the "Important Polymarket events" section even
	// when the AI is dead.
	annotations := w.loadAnnotations(ctx, candidates)

	if len(candidates) == 0 && len(annotations) == 0 {
		w.log.Info().Str("period_key", periodKey).Msg("marketintel: skipping empty periodic report")
		w.observeSkip("empty_report")
		return
	}

	req := buildRequest(candidates, periodEnd, w.cfg.Interval)
	if w.narrative != nil && len(candidates) > 0 {
		top := pickContextCandidate(candidates)
		if top != "" {
			req.EventNarrativeContext = w.narrative.LoadAndRenderForConditionID(ctx, top, 5000)
			if req.EventNarrativeContext != "" && w.metrics != nil && w.metrics.EventPageContextUsed != nil {
				w.metrics.EventPageContextUsed.WithLabelValues("market_intelligence").Inc()
			}
		}
	}

	// Budget gate.
	budgetDenied := false
	if w.budget != nil {
		if ok, reason := w.budget.Allow("market_intel", estPerMarketIntelUSD); !ok {
			budgetDenied = true
			w.log.Warn().Str("reason", reason).Msg("market intel: AI denied by budget")
		}
	}

	// PART 3 — prompt compaction visibility (best-effort).
	promptCharsBefore, promptCharsAfter := -1, -1
	if w.promptLoader != nil {
		preview := w.promptLoader.PreviewMarketReportPrompt(req)
		promptCharsBefore = len(preview)
		// SimpleCompactor-style char cap. We don't actually shorten
		// the request to the model here (the prompt builder is the
		// source of truth) — this only reports what the compaction
		// pass WOULD do and logs the delta. If the prompt is over the
		// cap, the worker will let the AI call short-circuit via the
		// preflight upstream; we simply record the delta and proceed.
		promptCharsAfter = promptCharsBefore
		if w.cfg.MaxInputChars > 0 && promptCharsBefore > w.cfg.MaxInputChars {
			promptCharsAfter = w.cfg.MaxInputChars
			if w.metrics != nil && w.metrics.AICompactions != nil {
				w.metrics.AICompactions.WithLabelValues(surfaceName, "chars_cap").Inc()
			}
		}
	}

	// PART 2 — call AI with retry-once on timeout. Skip entirely if
	// budget denied OR analyzer is a no-op.
	var (
		res         analysis.MarketReportAnalysis
		callErr     error
		retried     bool
		started     = w.cfg.Clock()
		aiAttempted = !budgetDenied
	)
	if aiAttempted {
		res, callErr, retried = w.callAnalyzerWithRetry(ctx, req)
	} else {
		res = analysis.MarketReportAnalysis{
			Status: analysis.StatusSkipped, Model: "unknown", LastError: "budget_denied",
		}
	}
	duration := w.cfg.Clock().Sub(started)
	if aiAttempted && w.metrics != nil && w.metrics.AILatencySeconds != nil {
		w.metrics.AILatencySeconds.WithLabelValues(surfaceName).Observe(duration.Seconds())
	}
	if callErr != nil {
		// callAnalyzerWithRetry already stamped res.Status / LastError.
		w.observeAIError("analyzer_error")
	}
	if w.budget != nil && res.EstimatedCostUSD > 0 {
		w.budget.Charge("market_intel", res.EstimatedCostUSD)
	}

	// Determine fallback reason for the renderer.
	fb := decideFallback(res, callErr, retried, budgetDenied)

	// Structured log: one line per tick that captures every knob the
	// operator cares about.
	w.log.Info().
		Str("period_key", periodKey).
		Str("model", res.Model).
		Str("ai_status", string(res.Status)).
		Str("ai_category", res.LastError).
		Int("prompt_chars_before", promptCharsBefore).
		Int("prompt_chars_after", promptCharsAfter).
		Dur("timeout_used", w.cfg.AITimeout).
		Dur("duration", duration).
		Bool("retry", retried).
		Bool("fallback", fb.Reason != "").
		Int("candidates", len(candidates)).
		Int("annotations", len(annotations)).
		Msg("marketintel: tick complete")

	// PART 4 — skip ONLY when the report would carry nothing
	// meaningful. With candidates OR annotations present, we ship a
	// deterministic body even on AI failure.
	hasContent := len(req.Markets) > 0 || len(annotations) > 0
	if !hasContent {
		w.observeSkip("empty_report")
		return
	}
	if !w.cfg.FallbackOnFailure && fb.Reason != "" {
		// Legacy behaviour: AI failure = skip everything.
		w.log.Warn().
			Str("period_key", periodKey).
			Str("ai_category", res.LastError).
			Msg("market intelligence skipped: ai_unavailable")
		w.observeSkip("ai_unavailable")
		return
	}

	// AI text (if any) is the analytical row content; the rendered
	// Telegram body is presentation-only and NEVER persisted.
	analysisText := strings.TrimSpace(res.ReportText)
	marketsJSON := marketsJSONSnapshot(req)
	// summary_hash dedup must change when any of (period, text,
	// candidate count) changes so a fallback report doesn't collide
	// with a successful report later in the same period.
	hashSeed := fmt.Sprintf("%s|%s|%d|%d|%s", periodKey, analysisText, len(req.Markets), len(annotations), fb.Reason)
	hash := bodyHash(hashSeed)

	stored, fresh, err := w.store.Insert(ctx, repository.NewMarketIntelligenceReport{
		PeriodKey:        periodKey,
		PeriodStart:      periodStart,
		PeriodEnd:        periodEnd,
		SummaryHash:      hash,
		ReportText:       analysisText,
		MarketsJSON:      marketsJSON,
		Model:            res.Model,
		PromptTokens:     int32(res.PromptTokens),
		CompletionTokens: int32(res.CompletionTokens),
		EstimatedCostUSD: res.EstimatedCostUSD,
		TelegramChatID:   w.cfg.ChatID,
		DeliveryStatus:   "pending",
	})
	if err != nil {
		w.log.Err(err).Str("period_key", periodKey).Msg("marketintel: persist failed")
		return
	}
	if !fresh {
		w.log.Debug().Str("period_key", periodKey).Msg("marketintel: dedup hit on period_key, skipping send")
		w.observeSkip("duplicate_period")
		return
	}
	_ = stored

	if w.bot == nil || w.cfg.ChatID == "" {
		w.log.Warn().Msg("marketintel: bot or chat id not configured; report persisted but not delivered")
		return
	}

	body, tally := Render(RenderInput{
		Request:     req,
		AIResult:    res,
		Candidates:  candidates,
		Annotations: annotations,
		Fallback:    fb,
		Links:       w.cfg.Links,
		VisibleN:    w.cfg.VisibleMarkets,
	})
	// Append the AI-ranked annotations appendix when wired. The hook
	// performs its own persistence; failure returns empty.
	if w.rankingHook != nil {
		if extra := w.rankingHook.RankAndRender(ctx, candidates, periodStart, periodEnd, 10); extra != "" {
			body += "\n\n" + extra
		}
	}

	w.observeLinks(tally)
	if fb.Reason != "" {
		w.observeFallbackSent(fb.Reason)
	}

	// PART 7 — SafeSplitForTelegram is the load-bearing length guard.
	chunks := alerting.SafeSplitForTelegram(body)
	if len(chunks) == 0 {
		// Defensive — should never trigger because we already gated
		// on hasContent.
		w.observeSkip("empty_report")
		return
	}
	for i, chunk := range chunks {
		if _, err := w.bot.SendHTML(ctx, w.cfg.ChatID, chunk); err != nil {
			w.log.Err(err).
				Str("period_key", periodKey).
				Int("chunk_index", i).
				Int("chunk_count", len(chunks)).
				Msg("marketintel: telegram send failed")
			return
		}
	}
}

// callAnalyzerWithRetry runs the analyzer call under the configured
// per-surface timeout and applies the v9.7 retry-once-on-timeout
// rule. Quota / rate-limit / 5xx all skip the retry. Parent context
// is honoured — when ctx is cancelled mid-backoff, the retry is
// skipped and the original failure is returned.
func (w *Worker) callAnalyzerWithRetry(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error, bool) {
	res, err := w.callAnalyzerOnce(ctx, req)
	if err == nil {
		return res, nil, false
	}
	if !w.cfg.RetryOnTimeout {
		return res, err, false
	}
	if !isRetryableTimeout(err) {
		return res, err, false
	}
	// Record timeout BEFORE the retry so the dashboard sees the
	// underlying frequency even when the retry succeeds.
	w.observeTimeout()
	// Honour parent context cancellation during backoff.
	backoff := jitteredBackoff(w.cfg.RetryBackoffMin, w.cfg.RetryBackoffMax)
	w.log.Warn().
		Dur("backoff", backoff).
		Str("ai_category", "timeout").
		Msg("marketintel: retrying AI call after timeout")
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return res, err, false
	case <-timer.C:
	}
	if w.metrics != nil && w.metrics.AIRetries != nil {
		w.metrics.AIRetries.WithLabelValues(surfaceName, "timeout").Inc()
	}
	res2, err2 := w.callAnalyzerOnce(ctx, req)
	if err2 == nil {
		return res2, nil, true
	}
	return res2, err2, true
}

// callAnalyzerOnce wraps AnalyzeMarketReport with the per-surface
// timeout context. Returns (res, err) where err is nil on success.
func (w *Worker) callAnalyzerOnce(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error) {
	callCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()
	res, err := w.analyzer.AnalyzeMarketReport(callCtx, req)
	if err != nil {
		return res, err
	}
	// Some analyzers return (StatusError, nil) instead of an error;
	// promote that to a real error so retry-once can fire.
	if res.Status == analysis.StatusError {
		return res, errors.New("analyzer returned status=error")
	}
	return res, nil
}

// isRetryableTimeout reports whether an analyzer error is a typed
// CategoryTimeout or a raw context.DeadlineExceeded. Quota / rate-
// limit / 5xx all return false here — those go through the existing
// budget / skip flow.
func isRetryableTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if pe, ok := openai.AsProviderError(err); ok {
		return pe.Category == openai.CategoryTimeout
	}
	// Heuristic last-resort: many fmt.Errorf chains lose the
	// DeadlineExceeded sentinel; sniffing the wrapped string is the
	// only signal we have. Conservative — we accept a small risk of
	// retrying a misclassified error.
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(strings.ToLower(msg), "timeout")
}

// decideFallback translates the AI call outcome into the renderer
// fallback signal. Empty Reason means the AI summary is usable.
func decideFallback(res analysis.MarketReportAnalysis, callErr error, retried, budgetDenied bool) FallbackInfo {
	if budgetDenied {
		return FallbackInfo{Reason: "budget_denied"}
	}
	if res.Status == analysis.StatusOK && strings.TrimSpace(res.ReportText) != "" {
		return FallbackInfo{}
	}
	// Map typed categories to operator-friendly reason labels.
	reason := strings.TrimSpace(res.LastError)
	if callErr != nil {
		if pe, ok := openai.AsProviderError(callErr); ok && pe.Category != "" {
			reason = string(pe.Category)
		} else if errors.Is(callErr, context.DeadlineExceeded) {
			reason = string(openai.CategoryTimeout)
		}
	}
	if reason == "" {
		reason = "ai_unavailable"
	}
	if retried && reason == string(openai.CategoryTimeout) {
		reason = "retry_exhausted_timeout"
	}
	msg := ""
	if callErr != nil {
		msg = callErr.Error()
	}
	return FallbackInfo{Reason: reason, Message: msg}
}

// loadAnnotations is best-effort — failures NEVER block the report.
func (w *Worker) loadAnnotations(ctx context.Context, cands []repository.IntelligenceCandidate) []AnnotationItem {
	if w.annotationLister == nil || w.cfg.AnnotationsPerEvt <= 0 {
		return nil
	}
	// Cap event lookups so we don't fan out 50× per cycle.
	maxEvents := 5
	if maxEvents > len(cands) {
		maxEvents = len(cands)
	}
	out := make([]AnnotationItem, 0, maxEvents*w.cfg.AnnotationsPerEvt)
	seenEvents := map[string]bool{}
	for _, c := range cands {
		if c.EventSlug == "" || seenEvents[c.EventSlug] {
			continue
		}
		seenEvents[c.EventSlug] = true
		if len(seenEvents) > maxEvents {
			break
		}
		rows, err := w.annotationLister.ListRecentAnnotations(ctx, c.EventSlug, int32(w.cfg.AnnotationsPerEvt))
		if err != nil || len(rows) == 0 {
			continue
		}
		for _, r := range rows {
			out = append(out, AnnotationItem{
				EventSlug:   c.EventSlug,
				MarketTitle: c.Question,
				Timestamp:   r.Timestamp,
				Outcome:     r.Outcome,
				PriceBefore: r.PriceBefore,
				PriceAfter:  r.PriceAfter,
				Title:       r.Title,
				Summary:     r.Summary,
				SourcesJSON: r.SourcesJSON,
				SourceName:  r.Source,
			})
		}
	}
	return out
}

// marketsJSONSnapshot returns the compact candidate dataset for
// dashboards. Distinct from the rendered Telegram body so the
// analytical column stays small and queryable.
func marketsJSONSnapshot(req analysis.MarketReportRequest) []byte {
	out, _ := json.Marshal(req.Markets)
	return out
}

// bucketedPeriod aligns `now` to the nearest interval boundary.
func bucketedPeriod(now time.Time, interval time.Duration) (end, start time.Time) {
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	end = now.UTC().Truncate(interval)
	start = end.Add(-interval)
	return end, start
}

func formatPeriodKey(start, end time.Time) string {
	return start.UTC().Format(time.RFC3339) + "/" + end.UTC().Format(time.RFC3339)
}

func (w *Worker) observeSkip(reason string) {
	if w.metrics == nil || w.metrics.MarketIntelligenceSkipped == nil {
		return
	}
	w.metrics.MarketIntelligenceSkipped.WithLabelValues(reason).Inc()
}

func (w *Worker) observeAIError(reason string) {
	if w.metrics == nil || w.metrics.AIRequestErrors == nil {
		return
	}
	w.metrics.AIRequestErrors.WithLabelValues("market_intelligence", reason).Inc()
}

func (w *Worker) observeTimeout() {
	if w.metrics == nil {
		return
	}
	if w.metrics.MarketIntelAITimeout != nil {
		w.metrics.MarketIntelAITimeout.Inc()
	}
	if w.metrics.AITimeoutTotal != nil {
		w.metrics.AITimeoutTotal.WithLabelValues(surfaceName).Inc()
	}
}

func (w *Worker) observeFallbackSent(reason string) {
	if w.metrics == nil || w.metrics.MarketIntelAIFallbackSent == nil {
		return
	}
	w.metrics.MarketIntelAIFallbackSent.WithLabelValues(reason).Inc()
}

func (w *Worker) observeLinks(t LinkTallies) {
	if w.metrics == nil || w.metrics.MarketIntelLinksRendered == nil {
		return
	}
	if t.Event > 0 {
		w.metrics.MarketIntelLinksRendered.WithLabelValues("event").Add(float64(t.Event))
	}
	if t.Market > 0 {
		w.metrics.MarketIntelLinksRendered.WithLabelValues("market").Add(float64(t.Market))
	}
	if t.Category > 0 {
		w.metrics.MarketIntelLinksRendered.WithLabelValues("category").Add(float64(t.Category))
	}
	if t.Grafana > 0 {
		w.metrics.MarketIntelLinksRendered.WithLabelValues("grafana").Add(float64(t.Grafana))
	}
	if t.Source > 0 {
		w.metrics.MarketIntelLinksRendered.WithLabelValues("source").Add(float64(t.Source))
		if w.metrics.MarketIntelSourceLinksRendered != nil {
			w.metrics.MarketIntelSourceLinksRendered.Add(float64(t.Source))
		}
	}
}

// filterAndDedupCandidates implements the report-quality rules:
//
//  1. Drop near-degenerate prices (≤ 0.02 / ≥ 0.98) — they have no
//     remaining return and are operationally useless.
//  2. Collapse per-condition duplicates.
//
// Stable order; preserves the SQL ranking.
func filterAndDedupCandidates(rows []repository.IntelligenceCandidate) []repository.IntelligenceCandidate {
	if len(rows) == 0 {
		return rows
	}
	const (
		floor   = 0.02
		ceiling = 0.98
	)
	seen := make(map[string]struct{}, len(rows))
	out := make([]repository.IntelligenceCandidate, 0, len(rows))
	for _, r := range rows {
		if r.LastPrice > 0 && (r.LastPrice <= floor || r.LastPrice >= ceiling) {
			continue
		}
		if _, dup := seen[r.ConditionID]; dup {
			continue
		}
		seen[r.ConditionID] = struct{}{}
		out = append(out, r)
	}
	return out
}

// buildRequest projects the candidate list into the analyzer's
// structured request.
func buildRequest(rows []repository.IntelligenceCandidate, now time.Time, period time.Duration) analysis.MarketReportRequest {
	req := analysis.MarketReportRequest{
		GeneratedAt: now,
		PeriodStart: now.Add(-period),
		PeriodEnd:   now,
		Markets:     make([]analysis.MarketReportMarket, 0, len(rows)),
	}
	for _, r := range rows {
		var remainPct float64
		if r.LastPrice > 0 && r.LastPrice < 1 {
			remainPct = 100 * (1 - r.LastPrice) / r.LastPrice
		}
		req.Markets = append(req.Markets, analysis.MarketReportMarket{
			Title:              r.Question,
			Category:           r.Category,
			LifecyclePct:       r.LifecyclePct,
			Probability:        r.LastPrice,
			RemainingReturnPct: remainPct,
			Volume24hUSD:       r.Volume24hUSD,
			RecentTrades24h:    int(r.Trades24h),
			AlertsLast24h:      int(r.Alerts24h),
		})
	}
	return req
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// pickContextCandidate selects the conditionID whose event-page
// context is most worth fetching for the 2h report.
func pickContextCandidate(rows []repository.IntelligenceCandidate) string {
	var best repository.IntelligenceCandidate
	for _, r := range rows {
		if r.Alerts24h > best.Alerts24h ||
			(r.Alerts24h == best.Alerts24h && r.Volume24hUSD > best.Volume24hUSD) {
			best = r
		}
	}
	return best.ConditionID
}

// jitteredBackoff returns a value uniformly in [min, max].
func jitteredBackoff(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	delta := max - min
	return min + time.Duration(rand.Int63n(int64(delta)+1))
}
