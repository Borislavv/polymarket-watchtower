// Package dailypoliticalintel runs the once-per-day political/
// geopolitical Polymarket intelligence report (v9.7 PART 4).
//
// On a time-of-day schedule (default 08:00 in
// DAILY_POLITICAL_INTEL_TIMEZONE) the worker:
//
//  1. selects up to MarketLimit candidate markets via
//     MarketIntelligenceRepository.ListIntelligenceCandidates,
//     filters by category whitelist (Politics/Geopolitics/Elections),
//     resolves event_slug per candidate, and dedupes;
//  2. fetches up to AnnotationsPerMarket newest annotations for
//     each event via the eventpagecontext.Provider;
//  3. attaches the active catalyst (if any) per event;
//  4. calls analysis.DailyPoliticalIntelGenerator for the verbatim
//     PART 5 prompt;
//  5. persists the row in polymarket_daily_political_intel_reports
//     (UNIQUE on report_date — same-day re-runs upsert);
//  6. sends the report to Telegram in section-aware splits if the
//     body exceeds the per-message cap.
//
// Failure semantics: AI failure marks the row delivery_status=
// "ai_failed" and leaves it for the next scheduled run; empty
// reports are never sent. Telegram failures mark "failed" with
// last_delivery_error. The alert pipeline is fully decoupled.
package dailypoliticalintel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config tunes the worker.
type Config struct {
	Enabled              bool
	TimeOfDay            string // "08:00"
	Timezone             string // "Europe/Tallinn"
	MarketLimit          int
	AnnotationsPerMarket int
	AIEnabled            bool
	AITimeout            time.Duration
	PromptMaxChars       int
	SendTelegram         bool
	ChatID               string
	CategoryWhitelist    []string

	// Telegram message cap. Default 3500 (under the 4096 hard cap)
	// leaves headroom for the header + section delimiters when the
	// renderer splits.
	TelegramMessageCap int

	// Clock is overridable for tests.
	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.TimeOfDay == "" {
		c.TimeOfDay = "08:00"
	}
	if c.Timezone == "" {
		c.Timezone = "Europe/Tallinn"
	}
	if c.MarketLimit <= 0 {
		c.MarketLimit = 100
	}
	if c.AnnotationsPerMarket <= 0 {
		c.AnnotationsPerMarket = 4
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 90 * time.Second
	}
	if c.PromptMaxChars <= 0 {
		c.PromptMaxChars = 30000
	}
	if c.TelegramMessageCap <= 0 {
		c.TelegramMessageCap = 3500
	}
	if len(c.CategoryWhitelist) == 0 {
		c.CategoryWhitelist = []string{"Politics", "Geopolitics", "Elections"}
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// CandidateSource is the seam to ListIntelligenceCandidates.
type CandidateSource interface {
	ListIntelligenceCandidates(ctx context.Context, limit int32) ([]repository.IntelligenceCandidate, error)
}

// MarketResolver maps a market conditionID to its event_slug.
type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

// EventPageRefresher refreshes the Polymarket event-page payload.
type EventPageRefresher interface {
	Load(ctx context.Context, eventSlug string, sev eventpagecontext.Severity) eventpagecontext.Summary
}

// CatalystSource exposes the active catalyst rows for an event slug.
type CatalystSource interface {
	ListActive(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
}

// FlowLoader is the optional seam to the eventflow aggregator. When
// wired, the daily worker populates the per-event FlowSummary so
// the AI prompt sees real alerts + trades + directional imbalance
// instead of empty fields. nil disables — the prompt's flow block
// renders the explicit "no meaningful stored flow" sentence.
type FlowLoader interface {
	LoadEventFlowSummary(ctx context.Context, eventSlug string, lookback time.Duration) (eventflow.EventFlowSummary, error)
}

// ReportStore persists daily reports.
type ReportStore interface {
	UpsertDailyReport(ctx context.Context, n repository.NewDailyPoliticalIntelReport) (int64, error)
	GetDailyReport(ctx context.Context, day time.Time) (repository.DailyPoliticalIntelReport, error)
}

// Telegram is the delivery seam. The worker calls SendHTML once
// per message-split chunk and records the returned MessageID.
type Telegram interface {
	SendHTML(ctx context.Context, chatID, text string) (TelegramResult, error)
}

// TelegramResult mirrors infra/telegram.SendResult without the
// dependency.
type TelegramResult struct {
	MessageID int64
}

// Worker is the periodic daily-report loop.
type Worker struct {
	cfg        Config
	candidates CandidateSource
	markets    MarketResolver
	pages      EventPageRefresher
	catalysts  CatalystSource
	store      ReportStore
	generator  analysis.DailyPoliticalIntelGenerator
	tg         Telegram
	flow       FlowLoader
	metrics    *metrics.Metrics
	log        *zerolog.Logger
}

// New wires the worker.
func New(
	cfg Config,
	candidates CandidateSource,
	markets MarketResolver,
	pages EventPageRefresher,
	catalysts CatalystSource,
	store ReportStore,
	generator analysis.DailyPoliticalIntelGenerator,
	tg Telegram,
	met *metrics.Metrics,
	log *zerolog.Logger,
) *Worker {
	cfg.applyDefaults()
	return &Worker{
		cfg:        cfg,
		candidates: candidates,
		markets:    markets,
		pages:      pages,
		catalysts:  catalysts,
		store:      store,
		generator:  generator,
		tg:         tg,
		metrics:    met,
		log:        log,
	}
}

// SetFlowLoader wires the optional eventflow aggregator. nil leaves
// the FlowSummary empty and the prompt renders the explicit
// "no meaningful stored flow" sentence.
func (w *Worker) SetFlowLoader(l FlowLoader) { w.flow = l }

// Run blocks until ctx cancels. The worker ticks every minute and
// fires the daily cycle when the wall-clock crosses the configured
// time-of-day for the configured timezone. Dedup-by-report_date in
// the store prevents same-day double-runs.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	tz, err := time.LoadLocation(w.cfg.Timezone)
	if err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Str("tz", w.cfg.Timezone).Msg("daily political intel: invalid timezone; defaulting to UTC")
		}
		tz = time.UTC
	}
	hh, mm, err := parseHHMM(w.cfg.TimeOfDay)
	if err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Str("time_of_day", w.cfg.TimeOfDay).Msg("daily political intel: invalid time-of-day; defaulting to 08:00")
		}
		hh, mm = 8, 0
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	var lastFiredDate time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := w.cfg.Clock().In(tz)
			if now.Hour() == hh && now.Minute() == mm {
				today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
				if !lastFiredDate.Equal(today) {
					w.Tick(ctx, today)
					lastFiredDate = today
				}
			}
		}
	}
}

// Tick runs one daily cycle for the supplied report_date. Exposed
// for tests + future CLI smoke. The store's UNIQUE(report_date)
// upsert means a same-day re-invoke is idempotent.
func (w *Worker) Tick(ctx context.Context, reportDate time.Time) {
	periodEnd := w.cfg.Clock()
	periodStart := periodEnd.Add(-24 * time.Hour)

	// Dedup: skip if today's row is already 'sent'. Failures are
	// re-runnable.
	if existing, err := w.store.GetDailyReport(ctx, reportDate); err == nil {
		if existing.DeliveryStatus == "sent" {
			w.observeReport("skipped")
			return
		}
	}

	markets, anns := w.selectMarkets(ctx)
	w.observeSelected(len(markets), anns)
	if len(markets) == 0 {
		w.observeReport("skipped")
		if w.log != nil {
			w.log.Info().Str("report_date", reportDate.Format("2006-01-02")).Msg("daily political intel: no candidate markets; skipping")
		}
		return
	}

	cats := w.collectCatalysts(ctx, markets)
	prevText := ""
	if prev, err := w.store.GetDailyReport(ctx, reportDate.AddDate(0, 0, -1)); err == nil {
		prevText = prev.AIReportText
	}

	flowAgg := w.aggregateFlow(ctx, markets)
	req := analysis.DailyPoliticalIntelRequest{
		ReportDate:         reportDate,
		PeriodStart:        periodStart,
		PeriodEnd:          periodEnd,
		Markets:            markets,
		FlowSummary:        flowAgg,
		KnownCatalysts:     cats,
		PreviousReportText: prevText,
	}

	row := repository.NewDailyPoliticalIntelReport{
		ReportDate:  reportDate,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
	}
	row.SelectedMarketsJSON, _ = json.Marshal(toMarketRefs(markets))
	row.SelectedAnnotationsJSON, _ = json.Marshal(collectAnnotationRefs(markets))
	row.CatalystsJSON, _ = json.Marshal(cats)

	if !w.cfg.AIEnabled {
		row.DeliveryStatus = "skipped"
		_, _ = w.store.UpsertDailyReport(ctx, row)
		w.observeReport("skipped")
		return
	}

	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()
	aiStart := w.cfg.Clock()
	res, err := w.generator.GenerateDailyPoliticalIntel(aiCtx, req)
	w.observeAILatency(w.cfg.Clock().Sub(aiStart))
	if err != nil || res.Status == analysis.StatusError {
		row.DeliveryStatus = "ai_failed"
		row.LastDeliveryError = sanitiseErr(err, res.LastError)
		_, _ = w.store.UpsertDailyReport(ctx, row)
		w.observeReport("ai_failed")
		if w.log != nil {
			w.log.Warn().Err(err).Str("report_date", reportDate.Format("2006-01-02")).Msg("daily political intel: AI generation failed")
		}
		return
	}
	if res.Status == analysis.StatusSkipped {
		row.DeliveryStatus = "skipped"
		row.LastDeliveryError = res.LastError
		_, _ = w.store.UpsertDailyReport(ctx, row)
		w.observeReport("skipped")
		return
	}
	if strings.TrimSpace(res.ReportText) == "" {
		row.DeliveryStatus = "ai_failed"
		row.LastDeliveryError = "empty_text"
		_, _ = w.store.UpsertDailyReport(ctx, row)
		w.observeReport("ai_failed")
		return
	}
	row.AIReportText = res.ReportText

	if !w.cfg.SendTelegram || w.tg == nil || strings.TrimSpace(w.cfg.ChatID) == "" {
		row.DeliveryStatus = "skipped"
		_, _ = w.store.UpsertDailyReport(ctx, row)
		w.observeReport("skipped")
		return
	}

	header := fmt.Sprintf("<b>Daily political market intelligence · %s</b>\n", reportDate.Format("2006-01-02"))
	body := header + "\n" + escapeReport(res.ReportText) + "\n\n" + RenderTopMarketsBlock(markets, 10)
	chunks := SplitTelegramBody(body, w.cfg.TelegramMessageCap)
	var ids []int64
	var sendErr error
	for _, c := range chunks {
		out, err := w.tg.SendHTML(ctx, w.cfg.ChatID, c)
		if err != nil {
			sendErr = err
			break
		}
		ids = append(ids, out.MessageID)
	}
	row.TelegramMessageIDsJSON, _ = json.Marshal(ids)
	if sendErr != nil {
		row.DeliveryStatus = "failed"
		row.LastDeliveryError = truncateErr(sendErr.Error())
		_, _ = w.store.UpsertDailyReport(ctx, row)
		w.observeReport("failed")
		if w.log != nil {
			w.log.Err(sendErr).Str("report_date", reportDate.Format("2006-01-02")).Msg("daily political intel: telegram send failed")
		}
		return
	}
	row.DeliveryStatus = "sent"
	_, _ = w.store.UpsertDailyReport(ctx, row)
	w.observeReport("sent")
	if w.log != nil {
		w.log.Info().
			Str("report_date", reportDate.Format("2006-01-02")).
			Int("markets", len(markets)).
			Int("annotations_passed", anns).
			Int("telegram_chunks", len(chunks)).
			Msg("daily political intel: report delivered")
	}
}

// --- selection -----------------------------------------------------------

// selectMarkets pulls candidates, filters by category, resolves
// event_slug, dedupes by event_slug, hydrates each with the newest
// AnnotationsPerMarket annotations from the event-page provider.
func (w *Worker) selectMarkets(ctx context.Context) ([]analysis.DailyIntelMarket, int) {
	candidateLimit := int32(w.cfg.MarketLimit * 3)
	rows, err := w.candidates.ListIntelligenceCandidates(ctx, candidateLimit)
	if err != nil {
		if w.log != nil {
			w.log.Err(err).Msg("daily political intel: list candidates failed")
		}
		return nil, 0
	}
	wl := lowerSet(w.cfg.CategoryWhitelist)
	seen := map[string]struct{}{}
	out := make([]analysis.DailyIntelMarket, 0, w.cfg.MarketLimit)
	totalAnns := 0

	for _, r := range rows {
		if len(out) >= w.cfg.MarketLimit {
			break
		}
		if !categoryAllowed(r.Category, wl) {
			continue
		}
		m, err := w.markets.GetByConditionID(ctx, r.ConditionID)
		if err != nil || strings.TrimSpace(m.EventSlug) == "" {
			continue
		}
		if _, dup := seen[m.EventSlug]; dup {
			continue
		}
		seen[m.EventSlug] = struct{}{}

		summary := w.pages.Load(ctx, m.EventSlug, eventpagecontext.SeverityInfo)
		annotations := pickNewest(summary.Annotations, w.cfg.AnnotationsPerMarket)
		totalAnns += len(annotations)

		var drift *float64
		if len(summary.Markets) > 0 && summary.Markets[0].OneDayPriceChange != nil {
			d := *summary.Markets[0].OneDayPriceChange
			drift = &d
		}
		out = append(out, analysis.DailyIntelMarket{
			EventSlug:         m.EventSlug,
			MarketSlug:        m.Slug,
			ConditionID:       r.ConditionID,
			Question:          r.Question,
			Category:          r.Category,
			LifecyclePct:      r.LifecyclePct,
			LastPrice:         r.LastPrice,
			OneDayPriceChange: drift,
			Volume24hUSD:      r.Volume24hUSD,
			AlertsLast24h:     r.Alerts24h,
			Annotations:       toRankingAnnotations(m.EventSlug, m.Slug, annotations),
		})
	}
	return out, totalAnns
}

// aggregateFlow walks the top selected markets, asks the FlowLoader
// for an EventFlowSummary per event, and folds them into one
// RankingFlowSummary the AI prompt consumes. Best-effort: empty
// when the loader isn't wired or every event returns empty.
func (w *Worker) aggregateFlow(ctx context.Context, markets []analysis.DailyIntelMarket) analysis.RankingFlowSummary {
	out := analysis.RankingFlowSummary{}
	if w.flow == nil {
		return out
	}
	// Bound the per-event calls so a 100-market day doesn't issue
	// 100 DB round-trips for the prompt block. We rank by alerts24h
	// already at selection time; the top 10 capture the loudest
	// events.
	const cap = 10
	seen := map[string]struct{}{}
	bestImbalance := 0.0
	for i, m := range markets {
		if i >= cap {
			break
		}
		if _, dup := seen[m.EventSlug]; dup {
			continue
		}
		seen[m.EventSlug] = struct{}{}
		sum, err := w.flow.LoadEventFlowSummary(ctx, m.EventSlug, 24*time.Hour)
		if err != nil {
			continue
		}
		if sum.Empty() {
			continue
		}
		out.RecentAlertsCount += sum.RecentAlerts
		out.SameSideNotional24h += sum.SameSideNotionalUSD
		out.OppositeSideNotional24h += sum.OppositeSideNotionalUSD
		if sum.LargestTradeUSD > out.LargestRecentTradeUSD {
			out.LargestRecentTradeUSD = sum.LargestTradeUSD
		}
		// Pick the strongest side across the highest-imbalance
		// event in the batch — gives the AI one anchor side to
		// reason about.
		if abs(sum.DirectionalImbalance) > abs(bestImbalance) {
			bestImbalance = sum.DirectionalImbalance
			if sum.StrongestSide != "" {
				out.StrongestSide = sum.StrongestSide + " on " + sum.StrongestOutcome
			}
		}
		if sum.AccumulationNote != "" && out.AccumulationNote == "" {
			out.AccumulationNote = sum.AccumulationNote
		}
		if sum.OwnershipNote != "" && out.OwnershipNote == "" {
			out.OwnershipNote = sum.OwnershipNote
		}
		if sum.ClusterNote != "" && out.ClusterNote == "" {
			out.ClusterNote = sum.ClusterNote
		}
	}
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func (w *Worker) collectCatalysts(ctx context.Context, markets []analysis.DailyIntelMarket) []analysis.DailyIntelCatalyst {
	if w.catalysts == nil {
		return nil
	}
	out := []analysis.DailyIntelCatalyst{}
	seen := map[string]struct{}{}
	for _, m := range markets {
		if _, dup := seen[m.EventSlug]; dup {
			continue
		}
		seen[m.EventSlug] = struct{}{}
		rows, err := w.catalysts.ListActive(ctx, m.EventSlug)
		if err != nil {
			continue
		}
		for _, r := range rows {
			out = append(out, analysis.DailyIntelCatalyst{
				EventSlug: r.EventSlug, CatalystType: string(r.CatalystType),
				Title: r.Title, ExpectedAt: r.ExpectedAt,
				Status: string(r.Status), Confidence: r.Confidence,
			})
		}
	}
	return out
}

// --- rendering helpers ---------------------------------------------------

// SplitTelegramBody splits a body string into pieces ≤ cap, breaking
// on blank-line section boundaries to avoid mid-section cuts.
// Exposed for tests.
func SplitTelegramBody(body string, cap int) []string {
	body = strings.TrimRight(body, "\n")
	if cap <= 0 || len(body) <= cap {
		return []string{body}
	}
	// Split into sections by blank-line separators ("\n\n").
	sections := strings.Split(body, "\n\n")
	var chunks []string
	var current strings.Builder
	for _, s := range sections {
		sep := "\n\n"
		if current.Len() == 0 {
			sep = ""
		}
		// If a single section is itself larger than cap, hard-split
		// by line — the alternative is a Telegram parse failure.
		if len(s) > cap {
			if current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
			}
			for _, line := range hardWrap(s, cap) {
				chunks = append(chunks, line)
			}
			continue
		}
		if current.Len()+len(sep)+len(s) > cap {
			chunks = append(chunks, current.String())
			current.Reset()
			current.WriteString(s)
			continue
		}
		current.WriteString(sep)
		current.WriteString(s)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func hardWrap(s string, cap int) []string {
	out := []string{}
	for len(s) > cap {
		out = append(out, s[:cap])
		s = s[cap:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// RenderTopMarketsBlock renders the "Top Polymarket events" list the
// daily Telegram message appends after the AI body. Exposed for
// tests + the worker.
func RenderTopMarketsBlock(markets []analysis.DailyIntelMarket, limit int) string {
	if len(markets) == 0 {
		return ""
	}
	if limit <= 0 || limit > len(markets) {
		limit = len(markets)
	}
	var b strings.Builder
	b.WriteString("<b>Top Polymarket events</b>\n")
	for i := 0; i < limit; i++ {
		m := markets[i]
		drift := ""
		if m.OneDayPriceChange != nil {
			drift = fmt.Sprintf(" · 24h=%+.2f", *m.OneDayPriceChange)
		}
		fmt.Fprintf(&b, "%d. %s · price=%.2f%s · vol24h=%.0f\n",
			i+1, escapeHTML(oneLine(m.Question)), m.LastPrice, drift, m.Volume24hUSD)
	}
	return b.String()
}

// --- metric helpers -----------------------------------------------------

func (w *Worker) observeReport(status string) {
	if w.metrics == nil || w.metrics.DailyPoliticalIntelReports == nil {
		return
	}
	w.metrics.DailyPoliticalIntelReports.WithLabelValues(status).Inc()
}

func (w *Worker) observeSelected(markets, anns int) {
	if w.metrics == nil {
		return
	}
	if w.metrics.DailyPoliticalIntelMarketsSelected != nil {
		w.metrics.DailyPoliticalIntelMarketsSelected.Add(float64(markets))
	}
	if w.metrics.DailyPoliticalIntelAnnotations != nil {
		w.metrics.DailyPoliticalIntelAnnotations.Add(float64(anns))
	}
}

func (w *Worker) observeAILatency(d time.Duration) {
	if w.metrics == nil || w.metrics.DailyPoliticalIntelAILatency == nil {
		return
	}
	w.metrics.DailyPoliticalIntelAILatency.Observe(d.Seconds())
}

// --- pure helpers -------------------------------------------------------

func parseHHMM(s string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	var hh, mm int
	_, err := fmt.Sscanf(parts[0], "%d", &hh)
	if err != nil {
		return 0, 0, err
	}
	_, err = fmt.Sscanf(parts[1], "%d", &mm)
	if err != nil {
		return 0, 0, err
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("out of range: %02d:%02d", hh, mm)
	}
	return hh, mm, nil
}

func lowerSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[strings.ToLower(strings.TrimSpace(s))] = struct{}{}
	}
	return out
}

func categoryAllowed(category string, wl map[string]struct{}) bool {
	if len(wl) == 0 {
		return true
	}
	lc := strings.ToLower(strings.TrimSpace(category))
	if lc == "" {
		return false
	}
	for needle := range wl {
		if strings.Contains(lc, needle) {
			return true
		}
	}
	return false
}

func pickNewest(rows []repository.EventAnnotation, n int) []repository.EventAnnotation {
	if n <= 0 || len(rows) == 0 {
		return nil
	}
	out := append([]repository.EventAnnotation(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func toRankingAnnotations(eventSlug, marketSlug string, rows []repository.EventAnnotation) []analysis.RankingAnnotation {
	out := make([]analysis.RankingAnnotation, 0, len(rows))
	for _, a := range rows {
		out = append(out, analysis.RankingAnnotation{
			EventSlug: eventSlug, MarketSlug: marketSlug,
			AnnotationHash: a.ItemHash,
			Timestamp:      a.Timestamp,
			Title:          a.Title, Summary: a.Summary, Outcome: a.Outcome,
			PriceBefore: a.PriceBefore, PriceAfter: a.PriceAfter, PriceChange: a.PriceChange,
		})
	}
	return out
}

// MarketRef is the small JSON shape persisted in
// selected_markets_json.
type MarketRef struct {
	EventSlug    string  `json:"event_slug"`
	MarketSlug   string  `json:"market_slug"`
	ConditionID  string  `json:"condition_id"`
	Question     string  `json:"question"`
	Category     string  `json:"category"`
	LastPrice    float64 `json:"last_price"`
	Volume24hUSD float64 `json:"volume_24h_usd"`
}

func toMarketRefs(markets []analysis.DailyIntelMarket) []MarketRef {
	out := make([]MarketRef, 0, len(markets))
	for _, m := range markets {
		out = append(out, MarketRef{
			EventSlug: m.EventSlug, MarketSlug: m.MarketSlug,
			ConditionID: m.ConditionID, Question: m.Question,
			Category: m.Category, LastPrice: m.LastPrice, Volume24hUSD: m.Volume24hUSD,
		})
	}
	return out
}

// AnnotationRef is the small JSON shape persisted in
// selected_annotations_json (one per annotation, deduped per event
// upstream).
type AnnotationRef struct {
	EventSlug   string  `json:"event_slug"`
	MarketSlug  string  `json:"market_slug"`
	Title       string  `json:"title"`
	Outcome     string  `json:"outcome"`
	Timestamp   string  `json:"timestamp,omitempty"`
	PriceChange float64 `json:"price_change,omitempty"`
}

func collectAnnotationRefs(markets []analysis.DailyIntelMarket) []AnnotationRef {
	out := []AnnotationRef{}
	for _, m := range markets {
		for _, a := range m.Annotations {
			pc := 0.0
			if a.PriceChange != nil {
				pc = *a.PriceChange
			}
			ts := ""
			if !a.Timestamp.IsZero() {
				ts = a.Timestamp.UTC().Format(time.RFC3339)
			}
			out = append(out, AnnotationRef{
				EventSlug: a.EventSlug, MarketSlug: a.MarketSlug,
				Title: a.Title, Outcome: a.Outcome, Timestamp: ts, PriceChange: pc,
			})
		}
	}
	return out
}

func sanitiseErr(err error, fallback string) string {
	if err != nil {
		return truncateErr(err.Error())
	}
	return truncateErr(fallback)
}

func truncateErr(s string) string {
	if len(s) > 500 {
		return s[:499] + "…"
	}
	return s
}

func escapeReport(s string) string {
	// Telegram HTML parse mode: escape <, >, & in free-text body.
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

// Compile-time checks against the seams.
var (
	_ sync.Mutex // pulled in via fakes in tests
)
