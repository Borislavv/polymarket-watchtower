// Package eventpagecontext owns the Polymarket event-page narrative
// pipeline: it fetches the hydrated /event/<slug>.json payload, persists
// the annotations + per-market snapshot, and renders a compact
// prompt-ready summary that aianalysis.Service stamps onto each
// AlertAnalysisRequest.
//
// Failure is silent: every fetch failure is logged + recorded on the
// fetch_state row, but the alert path NEVER blocks on it. When the
// payload is missing the renderer emits an "unavailable" slot that
// tells the model not to invent news.
//
// Polymarket-authored fields (event title, annotation summaries,
// source names) are passed through verbatim as DATA and MUST NOT be
// interpreted as instructions by the prompt builder.
package eventpagecontext

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config drives refresh + render policy.
type Config struct {
	Enabled          bool
	RefreshInfo      time.Duration // default 10m
	RefreshImportant time.Duration // default 5m (warning+/HOT)
	PromptMaxItems   int           // annotations shown in the prompt
	PromptMaxChars   int
	FetchTimeout     time.Duration
}

func (c *Config) applyDefaults() {
	if c.RefreshInfo <= 0 {
		c.RefreshInfo = 10 * time.Minute
	}
	if c.RefreshImportant <= 0 {
		c.RefreshImportant = 5 * time.Minute
	}
	if c.PromptMaxItems <= 0 {
		c.PromptMaxItems = 25
	}
	if c.PromptMaxChars <= 0 {
		c.PromptMaxChars = 5000
	}
	if c.FetchTimeout <= 0 {
		c.FetchTimeout = 8 * time.Second
	}
}

// Client is the seam to internal/infra/polymarket/eventpage.
type Client interface {
	FetchEventPage(ctx context.Context, eventSlug string) (*eventpage.EventPagePayload, error)
}

// Store is the persistence seam — *repository.EventPageRepository
// satisfies it.
type Store interface {
	InsertSnapshot(ctx context.Context, s repository.NewEventPageSnapshot) (int64, error)
	InsertMarket(ctx context.Context, m repository.NewEventPageMarket) error
	UpsertAnnotation(ctx context.Context, a repository.NewEventAnnotation) error
	FetchState(ctx context.Context, eventSlug string) (repository.EventPageFetchState, error)
	MarkFetch(ctx context.Context, eventSlug string, fetchedAt time.Time, buildID string, annCount int32, fetchErr string) error
	ListRecentAnnotations(ctx context.Context, eventSlug string, limit int32) ([]repository.EventAnnotation, error)
	ListLatestEventMarkets(ctx context.Context, eventSlug string) ([]repository.EventPageMarketRow, error)
}

// SlugResolver maps a market conditionID to its event_slug. The
// production adapter wraps *repository.MarketRepository.GetByConditionID
// (Market.EventSlug). Returning "" silently skips the context;
// callers must treat empty as "no narrative available".
type SlugResolver func(ctx context.Context, conditionID string) string

// Provider is the public facade.
type Provider struct {
	cfg      Config
	client   Client
	store    Store
	resolver SlugResolver
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time

	mu       sync.Mutex
	inFlight map[string]chan struct{}

	// eventMeta caches the most recent EventPageEvent per slug so the
	// renderer has title / context_description / resolution_rules
	// without a separate persistence layer. We deliberately skip a
	// dedicated polymarket_event_contexts table for the MVP — the
	// snapshot row already carries raw_json if a future consumer
	// needs to recover the data after a restart.
	eventMeta map[string]eventpage.EventPageEvent

	// unresolvedLogged is a throttle so we don't spam logs when the
	// same conditionId keeps failing slug resolution.
	unresolvedLogged map[string]time.Time
}

// New constructs a Provider.
func New(cfg Config, client Client, store Store, resolver SlugResolver, met *metrics.Metrics, log *zerolog.Logger) *Provider {
	cfg.applyDefaults()
	return &Provider{
		cfg:              cfg,
		client:           client,
		store:            store,
		resolver:         resolver,
		metrics:          met,
		log:              log,
		now:              time.Now,
		inFlight:         map[string]chan struct{}{},
		eventMeta:        map[string]eventpage.EventPageEvent{},
		unresolvedLogged: map[string]time.Time{},
	}
}

// LoadAndRenderForEventSlug is the slug-keyed convenience used by
// callers that already have the event slug. Side is intentionally
// empty — annotations render in time-descending order with no
// outcome bias.
func (p *Provider) LoadAndRenderForEventSlug(ctx context.Context, eventSlug string, maxChars int) string {
	if eventSlug == "" {
		return ""
	}
	return p.Load(ctx, eventSlug, SeverityInfo).Render("", p.cfg.PromptMaxItems, maxChars)
}

// LoadAndRenderForConditionID is the conditionID-keyed convenience
// used by the market-intelligence worker, which holds a market
// conditionID but not the event slug. The slug resolver wired into
// the provider does the conditionID → slug lookup.
func (p *Provider) LoadAndRenderForConditionID(ctx context.Context, conditionID string, maxChars int) string {
	if conditionID == "" || p.resolver == nil {
		return ""
	}
	slug := p.resolver(ctx, conditionID)
	if slug == "" {
		p.throttledUnresolvedLog(conditionID)
		return ""
	}
	return p.LoadAndRenderForEventSlug(ctx, slug, maxChars)
}

// StampRecentAnnotations satisfies the alertsender
// AlertAnnotationStamper seam. Resolves the event_slug from the
// Finding, loads up to 3 newest annotations (preferring the alert's
// outcome side first), and attaches them to f.RecentAnnotations so
// the Telegram formatter renders the "Recent annotations" block
// below the AI body. Failures degrade silently — the alert ships
// without the block.
func (p *Provider) StampRecentAnnotations(ctx context.Context, f *anomaly.Finding) {
	if f == nil || !p.cfg.Enabled || p.resolver == nil {
		return
	}
	conditionID := findingMarketID(*f)
	if conditionID == "" {
		return
	}
	slug := p.resolver(ctx, conditionID)
	if slug == "" {
		return
	}
	rows, err := p.store.ListRecentAnnotations(ctx, slug, 12)
	if err != nil || len(rows) == 0 {
		return
	}
	side := findingSide(*f)
	ordered := orderAnnotationsForSide(rows, side, 3)
	if len(ordered) == 0 {
		return
	}
	out := make([]anomaly.AnnotationRef, 0, len(ordered))
	for _, a := range ordered {
		out = append(out, anomaly.AnnotationRef{
			Title:       a.Title,
			Summary:     a.Summary,
			Outcome:     a.Outcome,
			Timestamp:   a.Timestamp,
			PriceBefore: a.PriceBefore,
			PriceAfter:  a.PriceAfter,
			PriceChange: a.PriceChange,
			SourceName:  a.Source,
		})
	}
	f.RecentAnnotations = out
}

// LoadAndRenderForFinding is the convenience method aianalysis.Service
// calls. Resolves the event slug, refreshes the cache per severity
// TTL, and returns the rendered prompt slot. Empty result = no usable
// context; the caller MUST not block on it.
func (p *Provider) LoadAndRenderForFinding(ctx context.Context, f anomaly.Finding, maxChars int) string {
	conditionID := findingMarketID(f)
	if conditionID == "" {
		return ""
	}
	if p.resolver == nil {
		return ""
	}
	slug := p.resolver(ctx, conditionID)
	if slug == "" {
		p.throttledUnresolvedLog(conditionID)
		return ""
	}
	side := findingSide(f)
	return p.Load(ctx, slug, severityFromFinding(f)).Render(side, p.cfg.PromptMaxItems, maxChars)
}

// Severity selects refresh TTL (mirror of marketcontext).
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
	SeverityHard     Severity = "hard"
	SeverityHotInfo  Severity = "hot_info"
)

// Summary is the compact view used by the renderer + lag detector.
type Summary struct {
	EventSlug     string
	LastFetchedAt time.Time
	ContextAge    time.Duration
	Stale         bool

	Event       eventpage.EventPageEvent
	Markets     []repository.EventPageMarketRow
	Annotations []repository.EventAnnotation
}

// Load returns the Summary, refreshing the cache per TTL when stale.
// Errors are swallowed; the worst case is a Summary with Stale=true.
func (p *Provider) Load(ctx context.Context, eventSlug string, sev Severity) Summary {
	if eventSlug == "" {
		return Summary{}
	}
	if !p.cfg.Enabled {
		return p.buildSummary(ctx, eventSlug, time.Time{})
	}
	ttl := p.refreshTTL(sev)
	state, _ := p.store.FetchState(ctx, eventSlug)
	lastFetchedAt := state.LastFetchedAt
	if p.now().Sub(lastFetchedAt) > ttl {
		p.refresh(ctx, eventSlug)
		state, _ = p.store.FetchState(ctx, eventSlug)
		lastFetchedAt = state.LastFetchedAt
	}
	return p.buildSummary(ctx, eventSlug, lastFetchedAt)
}

func (p *Provider) refreshTTL(sev Severity) time.Duration {
	switch sev {
	case SeverityWarning, SeverityCritical, SeverityHard, SeverityHotInfo:
		return p.cfg.RefreshImportant
	default:
		return p.cfg.RefreshInfo
	}
}

// refresh fetches the event page payload and persists it.
// Singleflight per event slug avoids stampedes.
func (p *Provider) refresh(ctx context.Context, eventSlug string) {
	if !p.acquire(eventSlug) {
		return
	}
	defer p.release(eventSlug)

	deadline, cancel := context.WithTimeout(ctx, p.cfg.FetchTimeout)
	defer cancel()
	startedAt := p.now()
	payload, err := p.client.FetchEventPage(deadline, eventSlug)
	latency := p.now().Sub(startedAt)
	p.observeFetchLatency(latency)
	if err != nil {
		_ = p.store.MarkFetch(ctx, eventSlug, startedAt, "", 0, truncErr(err.Error()))
		p.observeFetch("failed")
		// v10.5: classify typed *FetchError so the operator sees the
		// stable category (redirect_followed / stale_build_id /
		// canonical_slug_failed / ...) rather than a free-text error.
		// Existing annotation/catalyst rows are PRESERVED — refresh()
		// only WRITES new rows on success; it NEVER deletes on
		// failure. Downstream loaders fall back to whatever cached
		// snapshot is still in DB.
		fe, isTyped := eventpage.AsFetchError(err)
		if isTyped {
			p.observeContextStale(string(fe.Category))
		} else {
			p.observeContextStale("unknown")
		}
		if p.log != nil {
			ev := p.log.Warn().Err(err).Str("event_slug", eventSlug)
			if isTyped {
				ev = ev.
					Str("category", string(fe.Category)).
					Str("original_slug", fe.OriginalSlug).
					Str("canonical_slug", fe.CanonicalSlug).
					Str("build_id", fe.BuildID).
					Int("status", fe.Status).
					Str("location", fe.Location).
					Str("final_url", fe.FinalURL).
					Str("content_type", fe.ContentType).
					Int("retry_count", fe.RetryCount)
			}
			ev.Msg("event page: fetch failed (cached snapshot preserved)")
		}
		return
	}
	// Snapshot row owns per-market rows + annotations via FK on
	// snapshot_id only for markets; annotations have no FK to keep
	// dedup global per event_slug. raw_json is capped by the repo.
	rawJSON, _ := json.Marshal(payload)
	snapshotID, err := p.store.InsertSnapshot(ctx, repository.NewEventPageSnapshot{
		EventSlug: payload.EventSlug,
		BuildID:   payload.BuildID,
		FetchedAt: payload.FetchedAt,
		RawJSON:   rawJSON,
	})
	if err != nil {
		_ = p.store.MarkFetch(ctx, eventSlug, startedAt, payload.BuildID, 0, truncErr(err.Error()))
		p.observeFetch("persist_failed")
		if p.log != nil {
			p.log.Warn().Err(err).Str("event_slug", eventSlug).Msg("event page: snapshot insert failed")
		}
		return
	}
	for _, m := range payload.Markets {
		_ = p.store.InsertMarket(ctx, repository.NewEventPageMarket{
			SnapshotID:         snapshotID,
			EventSlug:          payload.EventSlug,
			MarketID:           m.MarketID,
			ConditionID:        m.ConditionID,
			MarketSlug:         m.Slug,
			Question:           m.Question,
			GroupItemTitle:     m.GroupItemTitle,
			Outcomes:           m.Outcomes,
			OutcomePrices:      m.OutcomePrices,
			Volume:             m.Volume,
			Volume24h:          m.Volume24h,
			Liquidity:          m.Liquidity,
			Active:             m.Active,
			Closed:             m.Closed,
			EndDate:            m.EndDate,
			OneHourPriceChange: m.OneHourPriceChange,
			OneDayPriceChange:  m.OneDayPriceChange,
			OneWeekPriceChange: m.OneWeekPriceChange,
			LastTradePrice:     m.LastTradePrice,
			BestBid:            m.BestBid,
			BestAsk:            m.BestAsk,
			CLOBTokenIDs:       m.CLOBTokenIDs,
			RawJSON:            m.RawJSON,
		})
	}
	for _, a := range payload.Annotations {
		sourcesJSON, _ := json.Marshal(a.Sources)
		var tweetsJSON []byte
		if len(a.Tweets) > 0 {
			tweetsJSON, _ = json.Marshal(a.Tweets)
		}
		_ = p.store.UpsertAnnotation(ctx, repository.NewEventAnnotation{
			EventSlug:   payload.EventSlug,
			ItemHash:    eventpage.AnnotationHash(a),
			Timestamp:   a.Timestamp,
			UnixTime:    a.UnixTime,
			TimeRange:   a.TimeRange,
			Title:       a.Title,
			Summary:     a.Summary,
			Outcome:     a.Outcome,
			PriceBefore: a.PriceBefore,
			PriceAfter:  a.PriceAfter,
			PriceChange: a.PriceChange,
			Source:      a.Source,
			SourcesJSON: sourcesJSON,
			TweetsJSON:  tweetsJSON,
			RawJSON:     a.RawJSON,
		})
	}
	_ = p.store.MarkFetch(ctx, eventSlug, startedAt, payload.BuildID, int32(len(payload.Annotations)), "")
	p.observeFetch("success")
	p.observeAnnotations(len(payload.Annotations))
	p.observeParseWarnings(payload)
	// Stash the event metadata so future buildSummary calls can
	// render title / context_description / resolution_rules
	// without re-fetching.
	p.mu.Lock()
	p.eventMeta[eventSlug] = payload.Event
	p.mu.Unlock()
	if p.log != nil {
		p.log.Debug().
			Str("event_slug", eventSlug).
			Str("build_id", payload.BuildID).
			Int("markets", len(payload.Markets)).
			Int("annotations", len(payload.Annotations)).
			Strs("query_keys", payload.RawQueryKeys).
			Msg("event page: fetch completed")
	}
}

func (p *Provider) acquire(slug string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.inFlight[slug]; busy {
		return false
	}
	p.inFlight[slug] = make(chan struct{})
	return true
}

func (p *Provider) release(slug string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch, ok := p.inFlight[slug]; ok {
		close(ch)
		delete(p.inFlight, slug)
	}
}

func (p *Provider) buildSummary(ctx context.Context, eventSlug string, lastFetchedAt time.Time) Summary {
	annotations, err := p.store.ListRecentAnnotations(ctx, eventSlug, int32(p.cfg.PromptMaxItems)*2)
	if err != nil {
		if p.log != nil {
			p.log.Warn().Err(err).Str("event_slug", eventSlug).Msg("event page: list annotations failed")
		}
	}
	markets, _ := p.store.ListLatestEventMarkets(ctx, eventSlug)
	p.mu.Lock()
	event := p.eventMeta[eventSlug]
	p.mu.Unlock()
	sum := Summary{
		EventSlug:     eventSlug,
		LastFetchedAt: lastFetchedAt,
		Event:         event,
		Markets:       markets,
		Annotations:   annotations,
	}
	if !lastFetchedAt.IsZero() {
		sum.ContextAge = p.now().Sub(lastFetchedAt)
	}
	sum.Stale = lastFetchedAt.IsZero() || sum.ContextAge > p.cfg.RefreshInfo
	return sum
}

func (p *Provider) throttledUnresolvedLog(conditionID string) {
	if p.log == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.unresolvedLogged[conditionID]; ok {
		if p.now().Sub(last) < time.Hour {
			return
		}
	}
	p.unresolvedLogged[conditionID] = p.now()
	p.log.Debug().Str("condition_id", conditionID).Msg("event page: unresolved event slug")
}

// --- metrics adapters -----------------------------------------------------

func (p *Provider) observeFetch(status string) {
	if p.metrics == nil || p.metrics.EventPageFetch == nil {
		return
	}
	p.metrics.EventPageFetch.WithLabelValues(status).Inc()
}

func (p *Provider) observeContextStale(reason string) {
	if p.metrics == nil || p.metrics.EventPageContextStale == nil {
		return
	}
	p.metrics.EventPageContextStale.WithLabelValues(reason).Inc()
}

func (p *Provider) observeAnnotations(n int) {
	if p.metrics == nil || p.metrics.EventPageAnnotations == nil {
		return
	}
	p.metrics.EventPageAnnotations.Add(float64(n))
}

func (p *Provider) observeFetchLatency(d time.Duration) {
	if p.metrics == nil || p.metrics.EventPageFetchLatency == nil {
		return
	}
	p.metrics.EventPageFetchLatency.Observe(d.Seconds())
}

// observeParseWarnings fans the parser's per-field drift signal out
// into Prometheus + one structured log line. Called after a successful
// fetch so the operator can correlate `parse_failures_total{field}`
// climbs with the specific event_slug + buildId that exhibited drift.
// Silent when the payload parsed cleanly.
func (p *Provider) observeParseWarnings(payload *eventpage.EventPagePayload) {
	if payload == nil {
		return
	}
	// Count per-market outcomes regardless of warnings — the
	// "ok" path is informative on its own (rate-of-markets-parsed).
	if p.metrics != nil && p.metrics.EventPageMarketParse != nil {
		p.metrics.EventPageMarketParse.WithLabelValues("ok").Add(float64(len(payload.Markets)))
		var skipped int
		for _, w := range payload.ParseWarnings {
			if w.Kind == "subobject_skipped" {
				skipped++
			}
		}
		if skipped > 0 {
			p.metrics.EventPageMarketParse.WithLabelValues("skipped").Add(float64(skipped))
		}
	}
	if len(payload.ParseWarnings) == 0 {
		return
	}
	if p.metrics != nil {
		if p.metrics.EventPagePartialParse != nil {
			p.metrics.EventPagePartialParse.Inc()
		}
		if p.metrics.EventPageParseFailures != nil {
			for _, w := range payload.ParseWarnings {
				p.metrics.EventPageParseFailures.WithLabelValues(w.Field).Inc()
			}
		}
	}
	if p.log != nil {
		// Cap log payload at the first 8 warnings — the operator
		// just needs to know which fields drifted; the prometheus
		// counters carry the full count.
		shown := payload.ParseWarnings
		if len(shown) > 8 {
			shown = shown[:8]
		}
		evt := p.log.Warn().
			Str("event_slug", payload.EventSlug).
			Str("build_id", payload.BuildID).
			Int("warnings_total", len(payload.ParseWarnings)).
			Int("markets_parsed", len(payload.Markets))
		for i, w := range shown {
			prefix := fmt.Sprintf("w%d_", i)
			evt = evt.Str(prefix+"field", w.Field).
				Str(prefix+"kind", w.Kind).
				Str(prefix+"type", w.OffendingType)
		}
		evt.Msg("event page: partial parse")
	}
}

// --- rendering ------------------------------------------------------------

// Render produces the prompt slot. side is the alert's BUY/SELL (used
// to prefer same-outcome annotations). maxItems caps the annotation
// count; maxChars caps the final string length.
func (s Summary) Render(side string, maxItems, maxChars int) string {
	if s.EventSlug == "" || (len(s.Annotations) == 0 && len(s.Markets) == 0) {
		return "Polymarket event page context: unavailable.\nDo not invent market news; reduce confidence."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Polymarket event page context:\n")
	fmt.Fprintf(&b, "event_slug: %s\n", s.EventSlug)
	if s.Event.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", oneLine(s.Event.Title))
	}
	if s.Event.ContextDescription != "" {
		fmt.Fprintf(&b, "context_description: %s\n", compact(s.Event.ContextDescription, 600))
	}
	if !s.Event.ContextUpdatedAt.IsZero() {
		fmt.Fprintf(&b, "context_updated_at: %s\n", s.Event.ContextUpdatedAt.UTC().Format(time.RFC3339))
	}
	if s.Event.ResolutionRules != "" {
		fmt.Fprintf(&b, "resolution: %s\n", compact(s.Event.ResolutionRules, 400))
	}
	if s.Stale {
		fmt.Fprintf(&b, "context_age: STALE (last_fetched=%s)\n", s.LastFetchedAt.Format(time.RFC3339))
	} else if !s.LastFetchedAt.IsZero() {
		fmt.Fprintf(&b, "context_age: %s (last_fetched=%s)\n", roundDur(s.ContextAge), s.LastFetchedAt.Format(time.RFC3339))
	}

	if len(s.Markets) > 0 {
		b.WriteString("event_markets:\n")
		for _, m := range s.Markets {
			price := ""
			if len(m.OutcomePrices) > 0 {
				price = m.OutcomePrices[0]
			}
			label := m.GroupItemTitle
			if label == "" {
				label = m.Question
			}
			drift := ""
			if m.OneDayPriceChange != nil {
				drift = fmt.Sprintf(" 24h=%+.2f", *m.OneDayPriceChange)
			}
			fmt.Fprintf(&b, "- %s | price=%s%s | vol24h=%.0f | condition=%s\n",
				oneLine(label), price, drift, m.Volume24h, m.ConditionID)
		}
	}

	if len(s.Annotations) > 0 {
		b.WriteString("annotations:\n")
		ordered := orderAnnotationsForSide(s.Annotations, side, maxItems)
		for _, a := range ordered {
			date := "—"
			if !a.Timestamp.IsZero() {
				date = a.Timestamp.UTC().Format("2006-01-02")
			}
			pricePart := ""
			if a.PriceBefore != nil && a.PriceAfter != nil {
				pricePart = fmt.Sprintf(" | price %.2f -> %.2f", *a.PriceBefore, *a.PriceAfter)
				if a.PriceChange != nil {
					pricePart += fmt.Sprintf(" (%+.2f)", *a.PriceChange)
				}
			} else if a.PriceChange != nil {
				pricePart = fmt.Sprintf(" | priceChange %+.2f", *a.PriceChange)
			}
			outcome := a.Outcome
			if outcome == "" {
				outcome = "—"
			}
			summary := compact(a.Summary, 300)
			sourceNames := sourcesNamesFromJSON(a.SourcesJSON)
			line := fmt.Sprintf("- %s | outcome=%s%s | %s | %s | sources=%s\n",
				date, outcome, pricePart, oneLine(a.Title), summary, sourceNames)
			b.WriteString(line)
		}
	}
	out := b.String()
	if maxChars > 0 && len(out) > maxChars {
		out = out[:maxChars-1] + "…"
	}
	return out
}

// orderAnnotationsForSide returns annotations with the alert's same-
// outcome items first (in time-descending order), then opposite-side
// items with large absolute priceChange, capped at maxItems.
func orderAnnotationsForSide(rows []repository.EventAnnotation, side string, maxItems int) []repository.EventAnnotation {
	if maxItems <= 0 {
		maxItems = len(rows)
	}
	// Stable sort by timestamp descending so cap-and-fill is
	// deterministic.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Timestamp.After(rows[j].Timestamp)
	})
	wantOutcome := strings.ToLower(strings.TrimSpace(side))
	out := make([]repository.EventAnnotation, 0, maxItems)
	if wantOutcome != "" {
		for _, a := range rows {
			if len(out) >= maxItems {
				break
			}
			if strings.EqualFold(a.Outcome, side) {
				out = append(out, a)
			}
		}
	}
	for _, a := range rows {
		if len(out) >= maxItems {
			break
		}
		// Skip already-picked same-outcome rows.
		if wantOutcome != "" && strings.EqualFold(a.Outcome, side) {
			continue
		}
		// Include opposite-side rows with material moves; include
		// neutral/unset-outcome rows always.
		if a.PriceChange != nil && absFloat(*a.PriceChange) >= 0.05 {
			out = append(out, a)
			continue
		}
		if a.Outcome == "" || wantOutcome == "" {
			out = append(out, a)
		}
	}
	return out
}

func sourcesNamesFromJSON(b []byte) string {
	if len(b) == 0 {
		return "—"
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) == 0 {
		return "—"
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, ", ")
}

// --- helpers --------------------------------------------------------------

func findingMarketID(f anomaly.Finding) string {
	if f.Trade != nil && f.Trade.Market != "" {
		return string(f.Trade.Market)
	}
	if f.Accumulation != nil && f.Accumulation.MarketID != "" {
		return f.Accumulation.MarketID
	}
	if f.Ownership != nil && f.Ownership.MarketID != "" {
		return f.Ownership.MarketID
	}
	if f.StableFavorite != nil && f.StableFavorite.MarketID != "" {
		return f.StableFavorite.MarketID
	}
	return ""
}

func findingSide(f anomaly.Finding) string {
	if f.Trade != nil && f.Trade.Outcome != "" {
		return f.Trade.Outcome
	}
	if f.Accumulation != nil && f.Accumulation.Outcome != "" {
		return f.Accumulation.Outcome
	}
	if f.StableFavorite != nil && f.StableFavorite.Outcome != "" {
		return f.StableFavorite.Outcome
	}
	return ""
}

func severityFromFinding(f anomaly.Finding) Severity {
	if f.Hot && f.Severity == anomaly.SeverityInfo {
		return SeverityHotInfo
	}
	switch f.Severity {
	case anomaly.SeverityWarning:
		return SeverityWarning
	case anomaly.SeverityCritical:
		return SeverityCritical
	case anomaly.SeverityHard:
		return SeverityHard
	default:
		return SeverityInfo
	}
}

func compact(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

func truncErr(s string) string {
	if len(s) > 200 {
		return s[:199] + "…"
	}
	return s
}

func roundDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
