// Package marketintel runs the 2h market-intelligence report.
//
// Pipeline:
//  1. List top-N candidate markets (lifecycle + recent activity + liquidity)
//  2. Build a compact MarketReportRequest
//  3. Call analyzer.AnalyzeMarketReport
//  4. Compose Telegram body (Overview / Markets to watch / What matters
//     next / Analyst summary)
//  5. Hash the body for dedup; INSERT ON CONFLICT (summary_hash) DO NOTHING
//  6. On fresh insert, post to Telegram and update delivery_status
//
// The worker is intentionally simple — the orchestration is mostly
// data shaping. The hard parts (model call, cost control, content
// hashing) live downstream in the analyzer + repo.
package marketintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
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

// Config tunes the worker.
type Config struct {
	Enabled        bool
	Interval       time.Duration
	MaxMarkets     int
	MaxOutputChars int
	ChatID         string

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

// Worker is the periodic 2h intelligence loop.
type Worker struct {
	cfg        Config
	candidates Candidates
	store      Store
	analyzer   Analyzer
	narrative  NarrativeLoader
	bot        Bot
	metrics    *metrics.Metrics
	log        *zerolog.Logger
}

// New wires the worker. All deps required. Pass analysis.NoopAnalyzer
// to disable the AI call (the worker still selects candidates and
// persists a "skipped" row so an operator can audit the cadence).
// Metrics is optional — when nil, observeSkip / observeAIError no-op.
func New(cfg Config, candidates Candidates, store Store, analyzer Analyzer, bot Bot, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{cfg: cfg, candidates: candidates, store: store, analyzer: analyzer, bot: bot, log: log}
}

// SetMetrics wires the optional metrics sink for skip/AI-error
// counters. Called once at boot; nil keeps the worker metrics-agnostic.
func (w *Worker) SetMetrics(m *metrics.Metrics) { w.metrics = m }

// SetNarrativeLoader wires the optional Polymarket event-page
// context loader. nil keeps the slot empty.
func (w *Worker) SetNarrativeLoader(loader NarrativeLoader) { w.narrative = loader }

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

	// Bucketed period — the load-bearing dedup primitive. Two ticks
	// inside the same Interval window resolve to the same
	// periodStart/periodEnd and the same period_key, so the second
	// INSERT collapses to ON CONFLICT DO NOTHING and no second
	// Telegram send happens. Without this, body.summary_hash differs
	// per tick (the body embeds the absolute `now` timestamp) and
	// the previous content-hash dedup never triggered.
	periodEnd, periodStart := bucketedPeriod(w.cfg.Clock(), w.cfg.Interval)
	periodKey := formatPeriodKey(periodStart, periodEnd)

	// Drop near-degenerate prices and per-market duplicates before
	// classification — see filterAndDedupCandidates for the rules.
	candidates = filterAndDedupCandidates(candidates)

	// Empty periodic reports are never sent: an "everything is quiet"
	// Telegram message every 2h is pure noise. Real Info/Warning/
	// Critical alerts still ship via the alertsender — this skip
	// applies ONLY to the periodic AI scout report.
	if len(candidates) == 0 {
		w.log.Info().
			Str("period_key", periodKey).
			Msg("marketintel: skipping empty periodic report")
		w.observeSkip("empty_report")
		return
	}

	req := buildRequest(candidates, periodEnd, w.cfg.Interval)
	// Stamp event-page context for the top-volume candidate. The
	// 2h report covers many markets, but a single event-page slot
	// keeps the prompt bounded — the candidate with the highest
	// alert load is the best signal of "what's moving right now".
	if w.narrative != nil && len(candidates) > 0 {
		top := pickContextCandidate(candidates)
		if top != "" {
			req.EventNarrativeContext = w.narrative.LoadAndRenderForConditionID(ctx, top, 5000)
			if req.EventNarrativeContext != "" && w.metrics != nil && w.metrics.EventPageContextUsed != nil {
				w.metrics.EventPageContextUsed.WithLabelValues("market_intelligence").Inc()
			}
		}
	}
	res, err := w.analyzer.AnalyzeMarketReport(ctx, req)
	if err != nil {
		w.log.Err(err).
			Str("period_key", periodKey).
			Msg("marketintel: analyzer returned error")
		res = analysis.MarketReportAnalysis{
			Status:    analysis.StatusError,
			Model:     "unknown",
			LastError: err.Error(),
		}
		w.observeAIError("analyzer_error")
	}

	// v8: when the AI is unavailable, do NOT ship a fake "AI summary
	// unavailable" message as if it were a normal report. The
	// periodic 2h intelligence report is an AI scout — without the
	// AI it is, by definition, not an intelligence report.
	//
	// Operators see this state through:
	//   - the structured log line below ("ai_unavailable: <category>")
	//   - watchtower_market_intelligence_skipped_total{reason="ai_unavailable"}
	//   - the polymarket_ai_request_logs row (separate operational
	//     telemetry table — see aianalysis.Service)
	if res.Status != analysis.StatusOK || strings.TrimSpace(res.ReportText) == "" {
		w.log.Warn().
			Str("period_key", periodKey).
			Str("ai_status", string(res.Status)).
			Str("ai_category", res.LastError).
			Msg("market intelligence skipped: ai_unavailable")
		w.observeSkip("ai_unavailable")
		return
	}

	// Persist ONLY the AI's analysis text — never the rendered
	// Telegram body. Rendering happens at send time below; storing
	// the rendered version would pollute the analytical table with
	// boilerplate (header / period: / Markets to watch / etc.).
	analysisText := strings.TrimSpace(res.ReportText)
	marketsJSON := marketsJSONSnapshot(req)
	hash := bodyHash(analysisText + "|" + periodKey)

	stored, fresh, err := w.store.Insert(ctx, repository.NewMarketIntelligenceReport{
		PeriodKey:        periodKey,
		PeriodStart:      periodStart,
		PeriodEnd:        periodEnd,
		SummaryHash:      hash,
		ReportText:       analysisText, // AI answer only — v8 contract.
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
		w.log.Debug().
			Str("period_key", periodKey).
			Msg("marketintel: dedup hit on period_key, skipping send")
		w.observeSkip("duplicate_period")
		return
	}
	_ = stored

	if w.bot == nil || w.cfg.ChatID == "" {
		w.log.Warn().Msg("marketintel: bot or chat id not configured; report persisted but not delivered")
		return
	}

	// Render the Telegram body AT SEND TIME from the request + the
	// AI text. The rendered string is NOT persisted.
	telegramBody := renderTelegramBody(req, res)
	if _, err := w.bot.SendHTML(ctx, w.cfg.ChatID, telegramBody); err != nil {
		w.log.Err(err).Str("period_key", periodKey).Msg("marketintel: telegram send failed")
		return
	}
}

// marketsJSONSnapshot returns the compact candidate dataset for
// dashboards. Distinct from the rendered Telegram body so the
// analytical column stays small and queryable.
func marketsJSONSnapshot(req analysis.MarketReportRequest) []byte {
	out, _ := json.Marshal(req.Markets)
	return out
}

// renderTelegramBody builds the Telegram HTML at send time from the
// request snapshot + the AI's analysis text. The result is NEVER
// persisted — this is purely a presentation layer.
func renderTelegramBody(req analysis.MarketReportRequest, res analysis.MarketReportAnalysis) string {
	body, _ := composeReport(req, res)
	return body
}

// bucketedPeriod aligns `now` to the nearest interval boundary so a
// 2h interval produces (10:00, 12:00) regardless of whether the tick
// fired at 10:00:01 or 10:01:30. The end of the period is the
// boundary AT OR BEFORE `now`; the start is end-interval. UTC because
// the period_key is a string and operators reading it across
// timezones must see the same value.
func bucketedPeriod(now time.Time, interval time.Duration) (end, start time.Time) {
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	end = now.UTC().Truncate(interval)
	start = end.Add(-interval)
	return end, start
}

// formatPeriodKey produces the deterministic string the UNIQUE index
// is built on. RFC3339 keeps it human-readable in Postgres so an
// operator looking at the table can correlate a row to a window
// without decoding.
func formatPeriodKey(start, end time.Time) string {
	return start.UTC().Format(time.RFC3339) + "/" + end.UTC().Format(time.RFC3339)
}

// observeSkip increments the metric so dashboards can chart how often
// periodic reports get suppressed and for which reason.
func (w *Worker) observeSkip(reason string) {
	if w.metrics == nil || w.metrics.MarketIntelligenceSkipped == nil {
		return
	}
	w.metrics.MarketIntelligenceSkipped.WithLabelValues(reason).Inc()
}

// observeAIError increments the AI-failure counter so the dashboard
// can chart the unavailable-summary rate.
func (w *Worker) observeAIError(reason string) {
	if w.metrics == nil || w.metrics.AIRequestErrors == nil {
		return
	}
	w.metrics.AIRequestErrors.WithLabelValues("market_intelligence", reason).Inc()
}

// filterAndDedupCandidates implements the report-quality rules:
//
//  1. Drop near-degenerate prices. A market trading at ≤ 0.02 or ≥
//     0.98 has effectively no remaining return (or no realistic flip
//     risk) and is operationally useless in a scout report. The
//     previous code surfaced these as "price 0.00 / price 1.00" rows
//     which the operator flagged as junk.
//  2. Collapse per-condition duplicates. The SQL query joins through
//     polymarket_market_categories which can fan a single market
//     into one row per category. Keep the first row per condition_id
//     so the visible list is a clean "markets to watch" feed, not a
//     join artefact.
//
// Stable: original order is preserved for the surviving rows so the
// SQL ORDER BY lifecycle / volume continues to drive the report.
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
// structured request. We don't pre-bucket whale-flow / stable
// favorite / asymmetric counts here — that bucketing requires
// cross-table reads we're deliberately deferring. The AI gets the
// raw market list + counters at zero and produces the operator-
// facing breakdown.
func buildRequest(rows []repository.IntelligenceCandidate, now time.Time, period time.Duration) analysis.MarketReportRequest {
	req := analysis.MarketReportRequest{
		GeneratedAt: now,
		PeriodStart: now.Add(-period),
		PeriodEnd:   now,
		Markets:     make([]analysis.MarketReportMarket, 0, len(rows)),
	}
	for _, r := range rows {
		// Remaining return: (1 - last_price) / last_price expressed
		// as a percentage. Zero when last_price is degenerate.
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

// composeReport renders the Telegram body in the exact shape the
// spec mandates. When the analyzer returned anything other than OK
// we still produce a body — operators want the candidate list and
// counts even when the AI summary itself is unavailable.
func composeReport(req analysis.MarketReportRequest, res analysis.MarketReportAnalysis) (string, []byte) {
	var b strings.Builder
	b.WriteString("<b>Market intelligence · 2h</b>\n")
	fmt.Fprintf(&b, "\nperiod: %s — %s\n",
		req.PeriodStart.Format(time.RFC3339), req.PeriodEnd.Format(time.RFC3339))

	b.WriteString("\n<b>Overview</b>\n")
	fmt.Fprintf(&b, "• markets evaluated: %d\n", len(req.Markets))
	fmt.Fprintf(&b, "• whale-flow candidates: %d\n", req.WhaleFlowCandidates)
	fmt.Fprintf(&b, "• stable favorites: %d\n", req.StableFavorites)
	fmt.Fprintf(&b, "• asymmetric setups: %d\n", req.AsymmetricSetups)
	fmt.Fprintf(&b, "• developing signals: %d\n", req.DevelopingSignals)

	b.WriteString("\n<b>Markets to watch</b>\n")
	n := len(req.Markets)
	if n > 8 {
		n = 8 // keep the visible list compact; the full list is in markets_json
	}
	for i := 0; i < n; i++ {
		m := req.Markets[i]
		fmt.Fprintf(&b, "%d. %s — lifecycle %.0f%%, price %.2f, vol24h $%.0f, alerts24h %d\n",
			i+1, htmlEscape(truncate(m.Title, 80)), m.LifecyclePct, m.Probability,
			m.Volume24hUSD, m.AlertsLast24h)
	}

	b.WriteString("\n<b>Analyst summary</b>\n")
	switch {
	case res.Status == analysis.StatusOK && res.ReportText != "":
		for i, line := range strings.Split(strings.TrimSpace(res.ReportText), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if i == 0 {
				fmt.Fprintf(&b, "• %s\n", htmlEscape(line))
				continue
			}
			fmt.Fprintf(&b, "  %s\n", htmlEscape(line))
		}
	case res.Status == analysis.StatusSkipped:
		b.WriteString("• AI summary unavailable (")
		b.WriteString(htmlEscape(res.LastError))
		b.WriteString("). Candidate list above is unranked.\n")
	default:
		b.WriteString("• AI summary unavailable. Candidate list above is unranked.\n")
	}

	// markets_json is the durable view of the candidates for later
	// SQL-side replay / strategy attribution.
	mj, _ := json.Marshal(req.Markets)
	return b.String(), mj
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// pickContextCandidate selects the conditionID whose event-page
// context is most worth fetching for the 2h report: first preference
// is the candidate with the highest Alerts24h count (the period
// surfaced it for a reason); ties break on Volume24hUSD.
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
