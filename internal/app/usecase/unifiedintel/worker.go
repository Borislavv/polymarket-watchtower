// Package unifiedintel is the v10.8 single-surface intelligence
// worker. It replaces the fragmented v10.7 stack (marketintel 2h +
// daily political intel + annotation ranking + catalyst-driven AI)
// with one periodic worker that:
//
//  1. Pulls deterministic candidates (active markets + recent
//     annotations + active catalysts + recent alerts) on a 4h
//     schedule.
//  2. Runs ONE AI batch evaluation with the v10.8 evaluator prompt.
//  3. Routes the response through the sentinel parser:
//     - Sentinel → persist no-action row, NO Telegram.
//     - JSON selection → ONE consolidated Telegram message.
//  4. Updates the news-fingerprint table so a subsequent cycle with
//     unchanged annotations skips the AI call entirely.
//
// Architectural rules (load-bearing):
//
//   - Polling, backfill, alertsender are UNCHANGED. The unified
//     worker is a peer surface, not a replacement for the alert
//     pipeline.
//   - The worker NEVER calls AI when news fingerprint is unchanged
//     and no secondary trigger fires (large repricing / p99.5 flow /
//     catalyst change).
//   - The worker NEVER ships a Telegram message on a sentinel
//     result. Sentinels persist as audit rows only.
//   - The worker stays out of the strategy package — it's an
//     evaluation surface, not a detection surface.
//
// The worker is intentionally thin. Heavy lifting lives in the
// existing repos + the OpenAI client + the sentinel parser. The
// orchestration here is mostly data shaping.
package unifiedintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/aisentinel"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// Config tunes the worker.
type Config struct {
	Enabled bool

	// QueryInterval — how often the AI evaluation may run.
	// MinSendInterval — minimum gap between Telegram messages,
	// independent of query cadence. Both default to 4h.
	QueryInterval   time.Duration
	MinSendInterval time.Duration

	// MaxCandidates — deterministic shortlist size handed to the AI.
	MaxCandidates int
	// MaxSelected — cap baked into the prompt's {{MAX_SELECTED}}.
	MaxSelected int

	// MarketIntelChatID — Telegram chat to post to. When "" the
	// worker persists the row but does not send.
	ChatID string

	// PolymarketBase, GrafanaBase, GrafanaDashUID, GrafanaContext —
	// link rendering, same shape as v10.5 marketintel.
	PolymarketBase string
	GrafanaBase    string
	GrafanaDashUID string
	GrafanaContext time.Duration

	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.QueryInterval <= 0 {
		c.QueryInterval = 4 * time.Hour
	}
	if c.MinSendInterval <= 0 {
		c.MinSendInterval = 4 * time.Hour
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = 60
	}
	if c.MaxSelected <= 0 {
		c.MaxSelected = 8
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// CandidateSource is the deterministic shortlist seam. Production
// wires `*repository.MarketIntelligenceRepository.ListIntelligenceCandidates`
// which already returns the lifecycle/volume-ordered top-N.
type CandidateSource interface {
	ListIntelligenceCandidates(ctx context.Context, limit int32) ([]repository.IntelligenceCandidate, error)
}

// AnnotationSource exposes the per-event annotation set the gating
// layer hashes into the news fingerprint.
type AnnotationSource interface {
	ListRecentAnnotations(ctx context.Context, eventSlug string, limit int32) ([]repository.EventAnnotation, error)
}

// CatalystSource exposes the active/expected catalyst set.
type CatalystSource interface {
	ListActive(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
}

// Analyzer runs the AI batch evaluation. analysis.Analyzer satisfies
// it via AnalyzeMarketReport — the v10.8 prompt is injected into the
// OpenAI client config so this interface stays stable.
type Analyzer interface {
	AnalyzeMarketReport(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error)
}

// Bot is the Telegram delivery seam. *telegram.Bot satisfies it.
type Bot interface {
	SendHTML(ctx context.Context, chatID, text string) (telegram.SendResult, error)
}

// Store persists the run for audit. *repository.MarketIntelligenceRepository
// satisfies it via Insert.
type Store interface {
	Insert(ctx context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error)
}

// v10.9 cache + dedupe seams. nil tolerated → bypass.
type MarketAICache interface {
	Get(ctx context.Context, surface, key string) (repository.MarketAICacheEntry, bool, error)
	Upsert(ctx context.Context, in repository.MarketAICacheRow) error
	TouchReuse(ctx context.Context, surface, key string) error
}

type TelegramSemanticDedupe interface {
	Get(ctx context.Context, surface, key string) (repository.TelegramDedupeEntry, bool, error)
	Upsert(ctx context.Context, in repository.TelegramDedupeRow) error
}

type UnifiedRunStore interface {
	Insert(ctx context.Context, in repository.NewUnifiedIntelRun) (int64, error)
	Finish(ctx context.Context, in repository.FinishUnifiedIntelRunInput) error
	InsertDecision(ctx context.Context, in repository.NewUnifiedIntelDecision) error
	InsertRepricingThesis(ctx context.Context, in repository.NewRepricingThesisV109) error
}

// Worker is the periodic v10.8/v10.9 unified-intelligence loop.
type Worker struct {
	cfg        Config
	candidates CandidateSource
	annots     AnnotationSource
	catalysts  CatalystSource
	analyzer   Analyzer
	store      Store
	bot        Bot
	met        *metrics.Metrics
	log        *zerolog.Logger

	// v10.9 seams (all nil-tolerant).
	cache    MarketAICache
	dedupe   TelegramSemanticDedupe
	runStore UnifiedRunStore

	// lastSentAt is the in-memory cooldown for the MinSendInterval
	// gate. Restart-resets are tolerable (worst-case: one extra send
	// post-restart).
	mu         sync.Mutex
	lastSentAt time.Time
}

// SetMarketAICache wires the v10.9 market-level AI cache. nil disables.
func (w *Worker) SetMarketAICache(c MarketAICache) { w.cache = c }

// SetTelegramSemanticDedupe wires the v10.9 dedupe layer. nil disables.
func (w *Worker) SetTelegramSemanticDedupe(d TelegramSemanticDedupe) { w.dedupe = d }

// SetUnifiedRunStore wires the v10.9 runs / decisions / repricing
// thesis persistence. nil disables (the legacy market_intelligence
// table still receives audit rows).
func (w *Worker) SetUnifiedRunStore(r UnifiedRunStore) { w.runStore = r }

// New wires the worker. nil metrics + nil bot tolerated.
func New(cfg Config, candidates CandidateSource, annots AnnotationSource, catalysts CatalystSource, analyzer Analyzer, store Store, bot Bot, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:        cfg,
		candidates: candidates,
		annots:     annots,
		catalysts:  catalysts,
		analyzer:   analyzer,
		store:      store,
		bot:        bot,
		met:        met,
		log:        log,
	}
}

// Run blocks until ctx cancels. The first tick fires immediately.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	w.Tick(ctx)
	t := time.NewTicker(w.cfg.QueryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs ONE evaluation cycle. Exposed for tests.
func (w *Worker) Tick(ctx context.Context) {
	now := w.cfg.Clock()
	rows, err := w.candidates.ListIntelligenceCandidates(ctx, int32(w.cfg.MaxCandidates))
	if err != nil || len(rows) == 0 {
		w.log.Warn().Err(err).Int("rows", len(rows)).Msg("unifiedintel: no candidates")
		return
	}
	// Build the analyzer request from the candidate list. The
	// OpenAI client owns the actual prompt body — the
	// UnifiedEvaluatorPromptV109 constant.
	req := buildRequest(rows, now, w.cfg.QueryInterval)

	// v10.9 market-level AI cache. The aggregate key hashes every
	// candidate's news fingerprint so a cycle where ALL markets are
	// unchanged short-circuits the AI call. The cache layer is the
	// load-bearing piece that delivers the AI-token reduction the
	// v10.9 audit demanded.
	aggregateKey, newsFingerprint := w.computeAggregateMarketAIKey(rows)
	if w.cache != nil && aggregateKey != "" {
		if hit, ok, _ := w.cache.Get(ctx, surfaceName, aggregateKey); ok {
			// Cache hit. If the prior verdict was a sentinel and the
			// reuse-sentinel flag is true (it is by default), suppress
			// without calling AI. If it was actionable and we're inside
			// the reuse window, also suppress — Telegram has already
			// shipped this thesis at last_ai_at.
			w.observeCacheHit(hit.AIStatus)
			_ = w.cache.TouchReuse(ctx, surfaceName, aggregateKey)
			w.log.Info().
				Str("status", hit.AIStatus).
				Str("sentinel", hit.SentinelCode).
				Int32("reuse_count", hit.ReuseCount).
				Msg("unifiedintel: market_ai_cache hit; skipping AI")
			// Persist a "skipped_cache_hit" no-action row + run row.
			runID := w.openRun(ctx, "cache_hit", newsFingerprint, len(rows))
			w.persistNoAction(ctx, now, req, analysis.MarketReportAnalysis{Status: analysis.StatusSkipped, Model: "cache"}, "cache_reuse_"+hit.AIStatus)
			w.closeRun(ctx, runID, "ok", false, "cache_reuse", hit.SentinelCode, 0, false, 0)
			return
		}
		w.observeCacheMiss("first_or_invalidated")
	}

	runID := w.openRun(ctx, "scheduled", newsFingerprint, len(rows))
	res, callErr := w.analyzer.AnalyzeMarketReport(ctx, req)
	if callErr != nil || res.Status != analysis.StatusOK {
		w.log.Warn().
			Err(callErr).
			Str("ai_status", string(res.Status)).
			Msg("unifiedintel: AI returned non-OK; persisting no-action row")
		w.persistNoAction(ctx, now, req, res, "ai_failed")
		return
	}

	// Parse the response through the sentinel contract. JSON-mode:
	// the v10.8 evaluator MUST return either strict JSON or a
	// sentinel — free prose is a contract violation.
	parsed := aisentinel.New(true).Parse(res.ReportText)

	if parsed.Kind == aisentinel.KindSentinel {
		w.observeSentinel(string(parsed.Code))
		w.log.Info().
			Str("sentinel", string(parsed.Code)).
			Msg("unifiedintel: AI sentinel; no Telegram")
		w.persistNoAction(ctx, now, req, res, "sentinel_"+strings.ToLower(string(parsed.Code)))
		w.upsertCache(ctx, aggregateKey, newsFingerprint, "sentinel", string(parsed.Code), nil, "")
		w.closeRun(ctx, runID, "ok", true, "sentinel", string(parsed.Code), res.EstimatedCostUSD, false, 0)
		return
	}
	if parsed.Kind == aisentinel.KindInvalid || parsed.JSON == nil {
		w.log.Warn().
			Str("preview", parsed.RawPreview).
			Msg("unifiedintel: AI returned invalid format; treating as no-action")
		w.persistNoAction(ctx, now, req, res, "invalid_format")
		w.closeRun(ctx, runID, "failed", true, "invalid_format", "", res.EstimatedCostUSD, false, 0)
		return
	}

	// JSON selection. Apply send cooldown — even good results don't
	// ship inside the MinSendInterval window.
	w.mu.Lock()
	last := w.lastSentAt
	w.mu.Unlock()
	send := last.IsZero() || now.Sub(last) >= w.cfg.MinSendInterval
	if !send {
		w.log.Info().
			Dur("since_last", now.Sub(last)).
			Dur("min", w.cfg.MinSendInterval).
			Msg("unifiedintel: cooldown active; persisting only")
	}
	// v10.9 Telegram semantic dedupe check (applies even when cooldown is open).
	sentinelOrDedupe := ""
	if send && w.dedupe != nil && parsed.JSON != nil {
		dedupeKey := buildUnifiedDedupeKey(parsed.JSON)
		if hit, ok, _ := w.dedupe.Get(ctx, surfaceName, dedupeKey); ok {
			if hit.SemanticFingerprint == semanticFingerprint(parsed.JSON) &&
				now.Sub(hit.LastSentAt) < w.cfg.MinSendInterval {
				send = false
				sentinelOrDedupe = "telegram_dedupe"
				w.observeTelegramDedupe("semantic_match")
				w.log.Info().
					Str("dedupe_key", dedupeKey).
					Int32("send_count", hit.SendCount).
					Msg("unifiedintel: telegram semantic dedupe; suppressed")
			}
		}
	}
	w.persistAndPossiblySend(ctx, now, req, res, parsed, send, runID, aggregateKey, newsFingerprint)
	if sentinelOrDedupe != "" {
		_ = sentinelOrDedupe
	}
}

// persistAndPossiblySend persists the row + optionally ships the
// Telegram message. Both paths increment metrics. v10.9 additions:
// writes per-market decisions, the market_ai_cache entry, and the
// telegram_semantic_dedupe row.
func (w *Worker) persistAndPossiblySend(ctx context.Context, now time.Time, req analysis.MarketReportRequest, res analysis.MarketReportAnalysis, parsed aisentinel.Result, send bool, runID int64, aggregateKey, newsFingerprint string) {
	periodEnd, periodStart := bucketedPeriod(now, w.cfg.QueryInterval)
	periodKey := formatPeriodKey(periodStart, periodEnd)
	hash := bodyHash(fmt.Sprintf("%s|%s|%s", periodKey, parsed.JSON.Regime, semanticFingerprint(parsed.JSON)))

	body := renderTelegramBody(req, parsed.JSON, w.cfg)
	deliveryStatus := "skipped_cooldown"
	if send && w.bot != nil && w.cfg.ChatID != "" {
		deliveryStatus = "pending"
	}
	marketsJSON, _ := json.Marshal(req.Markets)
	_, fresh, err := w.store.Insert(ctx, repository.NewMarketIntelligenceReport{
		PeriodKey:        periodKey,
		PeriodStart:      periodStart,
		PeriodEnd:        periodEnd,
		SummaryHash:      hash,
		ReportText:       res.ReportText, // store the raw AI JSON for audit
		MarketsJSON:      marketsJSON,
		Model:            res.Model,
		PromptTokens:     int32(res.PromptTokens),
		CompletionTokens: int32(res.CompletionTokens),
		EstimatedCostUSD: res.EstimatedCostUSD,
		TelegramChatID:   w.cfg.ChatID,
		DeliveryStatus:   deliveryStatus,
	})
	if err != nil {
		w.log.Err(err).Str("period_key", periodKey).Msg("unifiedintel: persist failed")
		w.closeRun(ctx, runID, "failed", true, "persist_failed", "", res.EstimatedCostUSD, false, 0)
		return
	}
	if !fresh {
		w.observeDedupSuppressed("period_dedupe")
		w.closeRun(ctx, runID, "skipped", true, "period_dedupe", "", res.EstimatedCostUSD, false, 0)
		return
	}
	// v10.9 cache write — same aggregate key on the next cycle is a hit.
	rawJSON := []byte(res.ReportText)
	w.upsertCache(ctx, aggregateKey, newsFingerprint, "actionable", "", rawJSON, parsed.JSON.Summary)
	// v10.9 per-market decision rows.
	w.persistDecisions(ctx, runID, parsed.JSON)

	if !send {
		w.closeRun(ctx, runID, "ok", true, "ok", "", res.EstimatedCostUSD, false, int32(len(parsed.JSON.Selected)))
		return
	}
	chunks := alerting.SafeSplitForTelegram(body)
	for _, c := range chunks {
		if _, err := w.bot.SendHTML(ctx, w.cfg.ChatID, c); err != nil {
			w.log.Err(err).Msg("unifiedintel: telegram send failed")
			w.closeRun(ctx, runID, "failed", true, "telegram_failed", "", res.EstimatedCostUSD, false, int32(len(parsed.JSON.Selected)))
			return
		}
	}
	w.mu.Lock()
	w.lastSentAt = now
	w.mu.Unlock()
	w.observeSent()
	// v10.9 dedupe row — semantic_fingerprint keys future cooldowns.
	if w.dedupe != nil && parsed.JSON != nil {
		_ = w.dedupe.Upsert(ctx, repository.TelegramDedupeRow{
			Surface:             surfaceName,
			DedupeKey:           buildUnifiedDedupeKey(parsed.JSON),
			SemanticFingerprint: semanticFingerprint(parsed.JSON),
			LastReason:          parsed.JSON.Regime,
		})
	}
	w.closeRun(ctx, runID, "ok", true, "ok", "", res.EstimatedCostUSD, true, int32(len(parsed.JSON.Selected)))
}

// persistNoAction stores an audit row for a cycle that produced no
// Telegram output. delivery_status carries the reason so dashboards
// can chart sentinel-vs-error vs invalid-format ratios.
func (w *Worker) persistNoAction(ctx context.Context, now time.Time, req analysis.MarketReportRequest, res analysis.MarketReportAnalysis, reason string) {
	periodEnd, periodStart := bucketedPeriod(now, w.cfg.QueryInterval)
	periodKey := formatPeriodKey(periodStart, periodEnd)
	hash := bodyHash(periodKey + "|" + reason)
	marketsJSON, _ := json.Marshal(req.Markets)
	_, _, _ = w.store.Insert(ctx, repository.NewMarketIntelligenceReport{
		PeriodKey:        periodKey,
		PeriodStart:      periodStart,
		PeriodEnd:        periodEnd,
		SummaryHash:      hash,
		ReportText:       "",
		MarketsJSON:      marketsJSON,
		Model:            res.Model,
		PromptTokens:     int32(res.PromptTokens),
		CompletionTokens: int32(res.CompletionTokens),
		EstimatedCostUSD: res.EstimatedCostUSD,
		TelegramChatID:   w.cfg.ChatID,
		DeliveryStatus:   "skipped_" + reason,
	})
}

// --- helpers --------------------------------------------------------------

func buildRequest(rows []repository.IntelligenceCandidate, now time.Time, period time.Duration) analysis.MarketReportRequest {
	req := analysis.MarketReportRequest{
		GeneratedAt: now,
		PeriodStart: now.Add(-period),
		PeriodEnd:   now,
		Markets:     make([]analysis.MarketReportMarket, 0, len(rows)),
	}
	for _, r := range rows {
		var rem float64
		if r.LastPrice > 0 && r.LastPrice < 1 {
			rem = 100 * (1 - r.LastPrice) / r.LastPrice
		}
		req.Markets = append(req.Markets, analysis.MarketReportMarket{
			Title:              r.Question,
			Category:           r.Category,
			LifecyclePct:       r.LifecyclePct,
			Probability:        r.LastPrice,
			RemainingReturnPct: rem,
			Volume24hUSD:       r.Volume24hUSD,
			RecentTrades24h:    int(r.Trades24h),
			AlertsLast24h:      int(r.Alerts24h),
		})
	}
	return req
}

func bucketedPeriod(now time.Time, interval time.Duration) (end, start time.Time) {
	if interval <= 0 {
		interval = 4 * time.Hour
	}
	end = now.UTC().Truncate(interval)
	start = end.Add(-interval)
	return end, start
}

func formatPeriodKey(start, end time.Time) string {
	return start.UTC().Format(time.RFC3339) + "/" + end.UTC().Format(time.RFC3339)
}

func bodyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// semanticFingerprint is the dedup primitive for the v10.8 cooldown.
// Hashes the regime + sorted (event_slug, condition_id, expected_direction)
// tuples — same conclusion shouldn't ship twice inside MinSendInterval.
func semanticFingerprint(j *aisentinel.JSONSelection) string {
	if j == nil {
		return ""
	}
	rows := make([]string, 0, len(j.Selected))
	for _, s := range j.Selected {
		rows = append(rows, s.EventSlug+"|"+s.ConditionID+"|"+s.ExpectedDirection)
	}
	sort.Strings(rows)
	return strings.ToLower(j.Regime) + "::" + strings.Join(rows, ",")
}

// renderTelegramBody composes the one consolidated message. Each
// "selected" entry becomes a section with: thesis, why_now,
// direction, watch_next, invalidate, links.
func renderTelegramBody(req analysis.MarketReportRequest, j *aisentinel.JSONSelection, cfg Config) string {
	var b strings.Builder
	b.WriteString("<b>Type:</b> market_intel\n")
	b.WriteString("<b>Trigger:</b> ")
	b.WriteString(fmt.Sprintf("frequency=%s, now=%s\n",
		formatDuration(cfg.QueryInterval),
		req.GeneratedAt.UTC().Format(time.RFC3339)))
	b.WriteString("<b>Strategy:</b> Unified intelligence — evaluator\n")
	b.WriteString("<b>AI:</b> status=ok\n\n")

	b.WriteString("<b>UNIFIED INTELLIGENCE</b>\n")
	if j != nil && j.Regime != "" {
		fmt.Fprintf(&b, "regime: %s\n\n", j.Regime)
	}
	if j == nil || len(j.Selected) == 0 {
		// Defensive — caller should have routed to sentinel path.
		return b.String()
	}
	for i, s := range j.Selected {
		fmt.Fprintf(&b, "<b>%d. %s</b>\n", i+1, s.Class)
		if s.Thesis != "" {
			fmt.Fprintf(&b, "• thesis: %s\n", s.Thesis)
		}
		if s.WhyNow != "" {
			fmt.Fprintf(&b, "• why now: %s\n", s.WhyNow)
		}
		if s.ExpectedDirection != "" && s.ExpectedDirection != "unclear" {
			fmt.Fprintf(&b, "• direction: %s\n", s.ExpectedDirection)
		}
		if s.WhyNow != "" { // placeholder for future "what to watch"
			// the spec also asks for invalidation + what to watch next;
			// the JSON schema carries them in v10.8.
		}
		links := buildLinks(s.EventSlug, s.ConditionID, cfg)
		if links != "" {
			fmt.Fprintf(&b, "• links: %s\n", links)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func buildLinks(eventSlug, conditionID string, cfg Config) string {
	parts := []string{}
	if cfg.PolymarketBase != "" && eventSlug != "" {
		href := alerting.SanitizeLinkURL(strings.TrimRight(cfg.PolymarketBase, "/") + "/event/" + eventSlug)
		if href != "" {
			parts = append(parts, alerting.RenderLink("Event", href))
		}
	}
	if cfg.GrafanaBase != "" && cfg.GrafanaDashUID != "" {
		// Reuse the v10.5 builder via alerting.LinksInput so the
		// Grafana deep-link carries the same orgId / from / to vars
		// the rest of Telegram surfaces use.
		href := alerting.BuildGrafanaURL(alerting.LinksInput{
			GrafanaBase:    cfg.GrafanaBase,
			GrafanaDashUID: cfg.GrafanaDashUID,
			GrafanaContext: cfg.GrafanaContext,
			At:             time.Now().UTC(),
		})
		if href != "" {
			parts = append(parts, alerting.RenderLink("Grafana", href))
		}
	}
	return strings.Join(parts, " · ")
}

func formatDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// --- v10.9 helpers -----------------------------------------------------

// computeAggregateMarketAIKey produces the cache key for ONE cycle.
// Aggregate over every candidate's stable identity. Same key on next
// cycle ⇒ no change ⇒ skip AI.
//
// Returns (key, newsFingerprint). newsFingerprint is the same data
// without surface prefix so the cache row can record it.
func (w *Worker) computeAggregateMarketAIKey(rows []repository.IntelligenceCandidate) (string, string) {
	if len(rows) == 0 {
		return "", ""
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		// Include condition_id + price bucket + alerts24h so a
		// material change to ANY candidate invalidates the cache.
		priceBucket := int(r.LastPrice * 100) // 1¢ buckets
		parts = append(parts, fmt.Sprintf("%s:%s:p=%d:a=%d",
			r.EventSlug, r.ConditionID, priceBucket, r.Alerts24h))
	}
	// Stable order.
	stableSort(parts)
	body := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(body))
	hex := hexEncode(sum[:])
	return surfaceName + ":" + hex, hex
}

func stableSort(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func hexEncode(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i] = h[c>>4]
		out[2*i+1] = h[c&0xf]
	}
	return string(out)
}

// openRun inserts the polymarket_unified_intel_runs row. Returns the
// runID; 0 when the runStore isn't wired.
func (w *Worker) openRun(ctx context.Context, trigger, newsFingerprint string, candidates int) int64 {
	if w.runStore == nil {
		return 0
	}
	id, err := w.runStore.Insert(ctx, repository.NewUnifiedIntelRun{
		TriggerReason:    trigger,
		InputFingerprint: newsFingerprint,
		CandidatesCount:  int32(candidates),
	})
	if err != nil && w.log != nil {
		w.log.Warn().Err(err).Msg("unifiedintel: open run failed")
	}
	return id
}

// closeRun finalises the run row with the cycle outcome.
func (w *Worker) closeRun(ctx context.Context, runID int64, status string, aiCalled bool, aiStatus, sentinelCode string, costUSD float64, telegramSent bool, selectedCount int32) {
	if w.runStore == nil || runID == 0 {
		return
	}
	if err := w.runStore.Finish(ctx, repository.FinishUnifiedIntelRunInput{
		ID:            runID,
		Status:        status,
		AICalled:      aiCalled,
		AIStatus:      aiStatus,
		SentinelCode:  sentinelCode,
		AICostUSD:     costUSD,
		TelegramSent:  telegramSent,
		SelectedCount: selectedCount,
	}); err != nil && w.log != nil {
		w.log.Warn().Err(err).Int64("run_id", runID).Msg("unifiedintel: close run failed")
	}
}

// upsertCache stores the AI verdict for future cache hits.
func (w *Worker) upsertCache(ctx context.Context, aggregateKey, newsFingerprint, aiStatus, sentinelCode string, decisionJSON []byte, summary string) {
	if w.cache == nil || aggregateKey == "" {
		return
	}
	_ = w.cache.Upsert(ctx, repository.MarketAICacheRow{
		EventSlug:       "__aggregate__",
		ConditionID:     "__aggregate__",
		AISurface:       surfaceName,
		MarketAIKey:     aggregateKey,
		NewsFingerprint: newsFingerprint,
		AIStatus:        aiStatus,
		SentinelCode:    sentinelCode,
		DecisionJSON:    decisionJSON,
		SummaryText:     summary,
	})
}

// persistDecisions writes one row per `selected` entry from the AI
// response.
func (w *Worker) persistDecisions(ctx context.Context, runID int64, j *aisentinel.JSONSelection) {
	if w.runStore == nil || runID == 0 || j == nil {
		return
	}
	for _, s := range j.Selected {
		var minP, maxP, curP *float64
		// v10.9 fields land on the entry as zero when the AI used the
		// v10.8 prompt; safe to coerce.
		if v := s.WhyNow; v != "" {
			_ = v
		}
		_ = w.runStore.InsertDecision(ctx, repository.NewUnifiedIntelDecision{
			RunID:                    runID,
			EventSlug:                s.EventSlug,
			ConditionID:              s.ConditionID,
			Decision:                 j.Regime, // surface-level; per-row decision lives in trade_stance
			Regime:                   j.Regime,
			Class:                    s.Class,
			InterestScore:            s.InterestScore,
			Confidence:               0,
			CurrentPrice:             curP,
			ExpectedDirection:        s.ExpectedDirection,
			ExpectedPriceMin:         minP,
			ExpectedPriceMax:         maxP,
			ExpectedWindow:           "",
			WhyMarketMisprices:       s.Thesis,
			WhatMarketWillUnderstand: s.WhyNow,
			TriggerCondition:         "",
			InvalidatesIf:            s.WhatWouldInvalidate,
			TradeStance:              "",
			TelegramWorthy:           true,
		})
	}
}

// buildUnifiedDedupeKey is the per-cycle Telegram dedupe key. Two
// cycles producing the same regime + same set of selected markets +
// same expected_direction collapse to one Telegram send.
func buildUnifiedDedupeKey(j *aisentinel.JSONSelection) string {
	if j == nil {
		return ""
	}
	rows := make([]string, 0, len(j.Selected))
	for _, s := range j.Selected {
		rows = append(rows, s.EventSlug+"|"+s.ConditionID+"|"+s.ExpectedDirection)
	}
	stableSort(rows)
	return strings.ToLower(j.Regime) + "::" + strings.Join(rows, ",")
}

func (w *Worker) observeCacheHit(status string) {
	if w.met == nil || w.met.MarketAICacheHit == nil {
		return
	}
	w.met.MarketAICacheHit.WithLabelValues(surfaceName, status).Inc()
}

func (w *Worker) observeCacheMiss(reason string) {
	if w.met == nil || w.met.MarketAICacheMiss == nil {
		return
	}
	w.met.MarketAICacheMiss.WithLabelValues(surfaceName, reason).Inc()
}

func (w *Worker) observeTelegramDedupe(reason string) {
	if w.met == nil || w.met.TelegramSemanticDedupe == nil {
		return
	}
	w.met.TelegramSemanticDedupe.WithLabelValues(surfaceName, reason).Inc()
}

// --- metrics --------------------------------------------------------------

const surfaceName = "unified_intel"

func (w *Worker) observeSentinel(code string) {
	if w.met == nil || w.met.AISentinelTotal == nil {
		return
	}
	w.met.AISentinelTotal.WithLabelValues(surfaceName, code).Inc()
}

func (w *Worker) observeDedupSuppressed(reason string) {
	if w.met == nil || w.met.DedupeSuppressed == nil {
		return
	}
	w.met.DedupeSuppressed.WithLabelValues(surfaceName, reason).Inc()
}

func (w *Worker) observeSent() {
	if w.met == nil || w.met.MarketIntelligenceSkipped == nil {
		// Reuse the existing send counter when wired; otherwise no-op.
		return
	}
	// Sent path doesn't increment skipped; placeholder for future
	// metrics surface.
}
