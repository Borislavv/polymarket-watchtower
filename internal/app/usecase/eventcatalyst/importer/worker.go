// Package importer is the periodic Political-Catalyst Intelligence
// extractor. Every Interval (default 5m) it:
//
//  1. Selects candidate Polymarket events from the
//     market-intelligence candidate query, filtered by category
//     whitelist (Politics / Geopolitics / Elections by default);
//  2. Refreshes the Polymarket event-page payload for each unique
//     event_slug via the existing eventpagecontext.Provider
//     (annotations + per-market snapshot land in DB);
//  3. Builds a structured CatalystExtractionRequest;
//  4. Calls the AI extractor (openai.Client satisfies
//     analysis.CatalystExtractor); validates strict-JSON output;
//  5. Upserts the catalysts (above MinConfidence) into
//     polymarket_event_catalysts;
//  6. Marks rows stale when they age past StaleAfter, are still in
//     (expected, active), and the AI did NOT re-emit them this
//     cycle; rows are never deleted by the importer.
//
// Failure of one event MUST NOT stop the batch. The alert flow is
// completely decoupled — Telegram delivery does not depend on the
// importer's status. Operators flip the entire feature off via
// EVENT_CATALYST_IMPORTER_ENABLED=false.
package importer

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config tunes the importer.
type Config struct {
	Enabled           bool
	Interval          time.Duration
	CategoryWhitelist []string
	BatchSize         int
	Concurrency       int
	Lookback          time.Duration
	AIEnabled         bool
	AITimeout         time.Duration
	MaxAnnotations    int
	MaxPromptChars    int
	MinConfidence     float64
	StaleAfter        time.Duration

	// CandidateLimit upper-bounds the row count we pull from
	// ListIntelligenceCandidates before category filtering. Defaults
	// to BatchSize * 4 — gives the category filter headroom.
	CandidateLimit int

	// Clock is overridable for tests.
	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	if len(c.CategoryWhitelist) == 0 {
		c.CategoryWhitelist = []string{"Politics", "Geopolitics", "Elections"}
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.Lookback <= 0 {
		c.Lookback = 48 * time.Hour
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 45 * time.Second
	}
	if c.MaxAnnotations <= 0 {
		c.MaxAnnotations = 40
	}
	if c.MaxPromptChars <= 0 {
		c.MaxPromptChars = 12000
	}
	if c.MinConfidence <= 0 {
		c.MinConfidence = 0.55
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 7 * 24 * time.Hour
	}
	if c.CandidateLimit <= 0 {
		c.CandidateLimit = c.BatchSize * 4
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// CandidateSource is the seam to ListIntelligenceCandidates.
// *repository.MarketIntelligenceRepository satisfies it.
type CandidateSource interface {
	ListIntelligenceCandidates(ctx context.Context, limit int32) ([]repository.IntelligenceCandidate, error)
}

// MarketResolver maps a market conditionID to its event_slug.
type MarketResolver interface {
	GetByConditionID(ctx context.Context, conditionID string) (repository.Market, error)
}

// EventPageRefresher refreshes the Polymarket event-page payload
// (annotations + markets + fetch state). *eventpagecontext.Provider
// satisfies it — Load() is the convenience method we lean on.
type EventPageRefresher interface {
	Load(ctx context.Context, eventSlug string, sev eventpagecontext.Severity) eventpagecontext.Summary
}

// CatalystStore is the persistence seam.
// *repository.EventCatalystRepository satisfies it.
type CatalystStore interface {
	Upsert(ctx context.Context, c repository.NewEventCatalyst) error
	ListActive(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
	ListAll(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
	SetStatus(ctx context.Context, id int64, status repository.EventCatalystStatus) error
}

// Worker is the periodic loop.
type Worker struct {
	cfg        Config
	candidates CandidateSource
	markets    MarketResolver
	pages      EventPageRefresher
	catalysts  CatalystStore
	extractor  analysis.CatalystExtractor
	metrics    *metrics.Metrics
	log        *zerolog.Logger
}

// New wires the worker. extractor may be analysis.NoopExtractor in
// dev mode — the worker still refreshes annotations but emits no
// catalyst rows (the AI step short-circuits to StatusSkipped).
func New(
	cfg Config,
	candidates CandidateSource,
	markets MarketResolver,
	pages EventPageRefresher,
	catalysts CatalystStore,
	extractor analysis.CatalystExtractor,
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
		extractor:  extractor,
		metrics:    met,
		log:        log,
	}
}

// Run blocks until ctx cancels. Performs an immediate tick so
// startup-queued events are processed without waiting one full
// interval.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick exposes one cycle for tests + the CLI smoke command.
func (w *Worker) Tick(ctx context.Context) {
	start := w.cfg.Clock()
	defer func() { w.observeLatency(time.Since(start)) }()

	slugs, selected, skippedNoSlug := w.selectEventSlugs(ctx)
	w.observeSelected(len(slugs))
	if w.log != nil {
		w.log.Info().
			Int("candidates", selected).
			Int("unique_event_slugs", len(slugs)).
			Int("skipped_no_slug", skippedNoSlug).
			Msg("event catalyst importer: cycle started")
	}
	if len(slugs) == 0 {
		w.observeCycle("empty")
		return
	}

	// Concurrent processing with bounded fan-out. Per-event failures
	// log + bump a metric and never affect siblings.
	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, slug := range slugs {
		slug := slug
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			status := w.processOne(ctx, slug)
			w.observeEventProcessed(status)
		}()
	}
	wg.Wait()
	w.observeCycle("ok")
	if w.log != nil {
		w.log.Info().
			Int("events", len(slugs)).
			Dur("duration", time.Since(start)).
			Msg("event catalyst importer: cycle completed")
	}
}

// processOne refreshes a single event, runs AI extraction, upserts
// catalysts, and marks stale rows. Returns a short status string
// for the events_processed metric.
func (w *Worker) processOne(ctx context.Context, eventSlug string) string {
	// Refresh the event-page payload. This persists annotations +
	// markets + fetch state via the existing eventpagecontext
	// machinery. Failure is silent at the provider boundary; the
	// returned Summary will be Stale=true.
	summary := w.pages.Load(ctx, eventSlug, eventpagecontext.SeverityInfo)
	if summary.EventSlug == "" {
		return "fetch_failed"
	}

	// Existing catalysts let the AI preserve / refine instead of
	// duplicating. ListAll so the model can decide to flip
	// resolved/stale rows back to active when new evidence arrives.
	existing, _ := w.catalysts.ListAll(ctx, eventSlug)

	if !w.cfg.AIEnabled {
		return "ai_disabled"
	}

	req := w.buildRequest(eventSlug, summary, existing)
	aiCtx, cancel := context.WithTimeout(ctx, w.cfg.AITimeout)
	defer cancel()
	res, err := w.extractor.ExtractCatalysts(aiCtx, req)
	if err != nil {
		w.observeAI("failed")
		if w.log != nil {
			w.log.Warn().Err(err).Str("event_slug", eventSlug).Msg("event catalyst importer: AI extraction failed")
		}
		return "ai_failed"
	}
	switch res.Status {
	case analysis.StatusSkipped:
		w.observeAI("skipped")
		return "ai_skipped"
	case analysis.StatusError:
		w.observeAI("failed")
		return "ai_failed"
	default:
		w.observeAI("ok")
	}

	freshTitles := w.upsertCatalysts(ctx, eventSlug, res.Catalysts)
	w.markStale(ctx, eventSlug, existing, freshTitles)
	if w.log != nil {
		w.log.Debug().
			Str("event_slug", eventSlug).
			Int("annotations", len(summary.Annotations)).
			Int("markets", len(summary.Markets)).
			Int("extracted", len(res.Catalysts)).
			Int("upserted", len(freshTitles)).
			Msg("event catalyst importer: event processed")
	}
	return "ok"
}

// upsertCatalysts writes every accepted catalyst row (above
// MinConfidence) and returns the normalised titles it persisted.
// The returned set powers the stale check.
func (w *Worker) upsertCatalysts(ctx context.Context, eventSlug string, rows []analysis.ExtractedCatalyst) map[string]struct{} {
	out := map[string]struct{}{}
	for _, c := range rows {
		if c.Confidence < w.cfg.MinConfidence {
			continue
		}
		expectedAt := time.Time{}
		if c.ExpectedAt != nil {
			if t, err := time.Parse(time.RFC3339, *c.ExpectedAt); err == nil {
				expectedAt = t.UTC()
			}
		}
		source := strDeref(c.Source)
		sourceURL := strDeref(strPtrSafe(c.SourceURL))
		newRow := repository.NewEventCatalyst{
			EventSlug:            eventSlug,
			CatalystType:         repository.EventCatalystType(c.CatalystType),
			Title:                strings.TrimSpace(c.Title),
			Description:          c.Description,
			ExpectedAt:           expectedAt,
			Confidence:           c.Confidence,
			Source:               source,
			SourceURL:            sourceURL,
			Status:               repository.EventCatalystStatus(c.Status),
			BullishScenario:      c.BullishScenario,
			BearishScenario:      c.BearishScenario,
			InvalidationScenario: c.InvalidationScenario,
		}
		if err := w.catalysts.Upsert(ctx, newRow); err != nil {
			w.observeUpsert("failed", string(newRow.CatalystType))
			if w.log != nil {
				w.log.Warn().Err(err).
					Str("event_slug", eventSlug).
					Str("catalyst_type", string(newRow.CatalystType)).
					Msg("event catalyst importer: upsert failed")
			}
			continue
		}
		w.observeUpsert(string(newRow.Status), string(newRow.CatalystType))
		out[normaliseTitle(newRow.Title)] = struct{}{}
	}
	return out
}

// markStale flips existing (expected, active) catalysts to stale
// when:
//   - the AI did NOT re-emit them this cycle, AND
//   - their last update is older than StaleAfter, AND
//   - expected_at is in the past (or unknown — we treat "no date" as
//     "operator-seeded with no anchor", which still ages out).
//
// Rows are NEVER deleted by the importer.
func (w *Worker) markStale(ctx context.Context, eventSlug string, existing []repository.EventCatalyst, freshTitles map[string]struct{}) {
	cutoff := w.cfg.Clock().Add(-w.cfg.StaleAfter)
	now := w.cfg.Clock()
	for _, c := range existing {
		if c.Status != repository.CatalystStatusExpected && c.Status != repository.CatalystStatusActive {
			continue
		}
		if _, fresh := freshTitles[normaliseTitle(c.Title)]; fresh {
			continue
		}
		if !c.UpdatedAt.Before(cutoff) {
			continue
		}
		if !c.ExpectedAt.IsZero() && c.ExpectedAt.After(now) {
			continue
		}
		if err := w.catalysts.SetStatus(ctx, c.ID, repository.CatalystStatusStale); err != nil {
			if w.log != nil {
				w.log.Warn().Err(err).
					Int64("id", c.ID).
					Str("event_slug", eventSlug).
					Msg("event catalyst importer: stale-mark failed")
			}
			continue
		}
		w.observeUpsert("stale", string(c.CatalystType))
	}
}

// buildRequest projects the event-page Summary + flow signals + the
// existing-catalyst ledger into the strict-JSON extraction request.
func (w *Worker) buildRequest(eventSlug string, summary eventpagecontext.Summary, existing []repository.EventCatalyst) analysis.CatalystExtractionRequest {
	req := analysis.CatalystExtractionRequest{
		EventSlug:       eventSlug,
		AnalysisTimeUTC: w.cfg.Clock().UTC(),
		EventMetadata: analysis.CatalystEventMetadata{
			Title:              summary.Event.Title,
			Description:        summary.Event.Description,
			ResolutionRules:    summary.Event.ResolutionRules,
			Category:           summary.Event.Category,
			StartDate:          summary.Event.StartDate,
			EndDate:            summary.Event.EndDate,
			ContextDescription: summary.Event.ContextDescription,
			ContextUpdatedAt:   summary.Event.ContextUpdatedAt,
		},
	}
	for _, m := range summary.Markets {
		req.Markets = append(req.Markets, analysis.CatalystMarket{
			ConditionID:        m.ConditionID,
			Question:           m.Question,
			GroupItemTitle:     m.GroupItemTitle,
			Outcomes:           m.Outcomes,
			OutcomePrices:      m.OutcomePrices,
			Volume24hUSD:       m.Volume24h,
			Liquidity:          m.Liquidity,
			OneHourPriceChange: m.OneHourPriceChange,
			OneDayPriceChange:  m.OneDayPriceChange,
			OneWeekPriceChange: m.OneWeekPriceChange,
			LastTradePrice:     m.LastTradePrice,
			Active:             m.Active,
			Closed:             m.Closed,
			EndDate:            m.EndDate,
		})
	}
	// Annotations: newest first, cap by MaxAnnotations.
	annotations := append([]repository.EventAnnotation(nil), summary.Annotations...)
	sort.SliceStable(annotations, func(i, j int) bool {
		return annotations[i].Timestamp.After(annotations[j].Timestamp)
	})
	if w.cfg.MaxAnnotations > 0 && len(annotations) > w.cfg.MaxAnnotations {
		annotations = annotations[:w.cfg.MaxAnnotations]
	}
	for _, a := range annotations {
		req.Annotations = append(req.Annotations, analysis.CatalystAnnotation{
			Timestamp:   a.Timestamp,
			Title:       a.Title,
			Summary:     a.Summary,
			Outcome:     a.Outcome,
			PriceBefore: a.PriceBefore,
			PriceAfter:  a.PriceAfter,
			PriceChange: a.PriceChange,
			SourceNames: nil, // sources_json lives in raw; not denormalised today
		})
	}
	for _, c := range existing {
		req.ExistingCatalysts = append(req.ExistingCatalysts, analysis.CatalystExistingRow{
			CatalystType: string(c.CatalystType),
			Title:        c.Title,
			ExpectedAt:   c.ExpectedAt,
			Status:       string(c.Status),
			Confidence:   c.Confidence,
		})
	}
	return req
}

// selectEventSlugs pulls the candidate list, filters by category,
// resolves event_slug per conditionID, and dedupes. Caller logs the
// selected/skipped counts.
func (w *Worker) selectEventSlugs(ctx context.Context) (slugs []string, selected int, skippedNoSlug int) {
	rows, err := w.candidates.ListIntelligenceCandidates(ctx, int32(w.cfg.CandidateLimit))
	if err != nil {
		if w.log != nil {
			w.log.Err(err).Msg("event catalyst importer: list candidates failed")
		}
		return nil, 0, 0
	}
	wl := lowerSet(w.cfg.CategoryWhitelist)
	seen := map[string]struct{}{}
	// Stable priority: alerts24h desc, then volume24h desc — done in
	// SQL already by ListIntelligenceCandidates, so we iterate in
	// source order.
	for _, r := range rows {
		if !categoryAllowed(r.Category, wl) {
			continue
		}
		selected++
		m, err := w.markets.GetByConditionID(ctx, r.ConditionID)
		if err != nil || strings.TrimSpace(m.EventSlug) == "" {
			skippedNoSlug++
			continue
		}
		if _, dup := seen[m.EventSlug]; dup {
			continue
		}
		seen[m.EventSlug] = struct{}{}
		slugs = append(slugs, m.EventSlug)
		if len(slugs) >= w.cfg.BatchSize {
			break
		}
	}
	return slugs, selected, skippedNoSlug
}

// --- metrics + helpers --------------------------------------------------

func (w *Worker) observeCycle(status string) {
	if w.metrics == nil || w.metrics.EventCatalystImporterRuns == nil {
		return
	}
	w.metrics.EventCatalystImporterRuns.WithLabelValues(status).Inc()
}

func (w *Worker) observeSelected(n int) {
	if w.metrics == nil || w.metrics.EventCatalystImporterSelected == nil {
		return
	}
	w.metrics.EventCatalystImporterSelected.Add(float64(n))
}

func (w *Worker) observeEventProcessed(status string) {
	if w.metrics == nil || w.metrics.EventCatalystImporterProcessed == nil {
		return
	}
	w.metrics.EventCatalystImporterProcessed.WithLabelValues(status).Inc()
}

func (w *Worker) observeAI(status string) {
	if w.metrics == nil || w.metrics.EventCatalystAIRequests == nil {
		return
	}
	w.metrics.EventCatalystAIRequests.WithLabelValues(status).Inc()
}

func (w *Worker) observeUpsert(status, catalystType string) {
	if w.metrics == nil || w.metrics.EventCatalystUpserted == nil {
		return
	}
	w.metrics.EventCatalystUpserted.WithLabelValues(status, catalystType).Inc()
}

func (w *Worker) observeLatency(d time.Duration) {
	if w.metrics == nil || w.metrics.EventCatalystImportLatency == nil {
		return
	}
	w.metrics.EventCatalystImportLatency.Observe(d.Seconds())
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

func normaliseTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func strDeref(s string) string { return s }

func strPtrSafe(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
