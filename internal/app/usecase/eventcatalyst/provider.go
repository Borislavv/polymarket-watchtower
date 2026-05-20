// Package eventcatalyst is the Political-Catalyst Intelligence
// overlay. It owns:
//
//   - reads against polymarket_event_catalysts (the persisted future
//     events the market is structurally waiting on: runoffs, debates,
//     rulings, certifications, sanctions votes, etc.);
//   - the rendered "Future catalysts:" prompt block AI consumes;
//   - the rendered "Blocked Alert" payload the Telegram formatter
//     emits ABOVE the AI body when at least one catalyst is active;
//   - the loader seam aianalysis.Service + alertsender bind to.
//
// Catalysts are NOT a standalone strategy — they are a cross-strategy
// intelligence overlay. They modify interpretation of accumulation,
// whale-flow, ownership-concentration, stable-favorite, cluster, and
// low-baseline findings via the prompt context and the Telegram
// block. Failure NEVER blocks alert delivery.
//
// Polymarket-authored / AI-authored content stored in this table is
// DATA. Renderers MUST treat it as evidence, never as instructions.
package eventcatalyst

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Config tunes loader behaviour.
type Config struct {
	Enabled        bool
	PromptMaxItems int
	PromptMaxChars int
}

func (c *Config) applyDefaults() {
	if c.PromptMaxItems <= 0 {
		c.PromptMaxItems = 6
	}
	if c.PromptMaxChars <= 0 {
		c.PromptMaxChars = 2000
	}
}

// Store is the persistence seam. *repository.EventCatalystRepository
// satisfies it.
type Store interface {
	ListActive(ctx context.Context, eventSlug string) ([]repository.EventCatalyst, error)
}

// SlugResolver maps a market conditionID to its event_slug. Production
// adapter wraps *repository.MarketRepository.GetByConditionID.
type SlugResolver func(ctx context.Context, conditionID string) string

// Provider is the public facade.
type Provider struct {
	cfg      Config
	store    Store
	resolver SlugResolver
	metrics  *metrics.Metrics
	log      *zerolog.Logger
	now      func() time.Time

	mu               sync.Mutex
	unresolvedLogged map[string]time.Time
}

// New constructs the provider.
func New(cfg Config, store Store, resolver SlugResolver, met *metrics.Metrics, log *zerolog.Logger) *Provider {
	cfg.applyDefaults()
	return &Provider{
		cfg:              cfg,
		store:            store,
		resolver:         resolver,
		metrics:          met,
		log:              log,
		now:              time.Now,
		unresolvedLogged: map[string]time.Time{},
	}
}

// LoadActiveForFinding resolves the event slug from the Finding's
// market and returns the active catalysts. Returns nil on slug-
// unresolved / store error / disabled — callers MUST treat nil as
// "no catalyst context" and the prompt/formatter renders the empty
// fallback. NEVER blocks the alert path.
func (p *Provider) LoadActiveForFinding(ctx context.Context, f anomaly.Finding) []repository.EventCatalyst {
	if !p.cfg.Enabled || p.resolver == nil || p.store == nil {
		return nil
	}
	conditionID := findingMarketID(f)
	if conditionID == "" {
		return nil
	}
	slug := p.resolver(ctx, conditionID)
	if slug == "" {
		p.throttledUnresolvedLog(conditionID)
		return nil
	}
	rows, err := p.store.ListActive(ctx, slug)
	if err != nil {
		if p.log != nil {
			p.log.Warn().Err(err).Str("event_slug", slug).Msg("event catalyst: list active failed")
		}
		return nil
	}
	return rows
}

// LoadAndRenderForFinding satisfies the aianalysis CatalystLoader
// seam: returns the rendered "Future catalysts:" prompt block. Empty
// string means "no catalyst context"; aianalysis falls back to a
// stable "no catalyst recorded" sentence.
func (p *Provider) LoadAndRenderForFinding(ctx context.Context, f anomaly.Finding, maxChars int) string {
	rows := p.LoadActiveForFinding(ctx, f)
	if len(rows) == 0 {
		return ""
	}
	return RenderCatalystPromptBlock(rows, p.cfg.PromptMaxItems, maxChars)
}

// StampBlockedAlert satisfies the alertsender.BlockedAlertStamper
// seam. It picks the most relevant active catalyst for the Finding's
// event and attaches a BlockedAlert payload to f.Blocked so the
// Telegram formatter can render the "Blocked Alert" block ABOVE the
// AI analysis. No-op when there are no active catalysts.
func (p *Provider) StampBlockedAlert(ctx context.Context, f *anomaly.Finding) {
	if f == nil {
		return
	}
	rows := p.LoadActiveForFinding(ctx, *f)
	if len(rows) == 0 {
		return
	}
	primary, ok := PrimaryCatalyst(rows)
	if !ok {
		return
	}
	f.Blocked = blockedFromCatalyst(primary)
}

// blockedFromCatalyst projects a catalyst row into the operator-
// facing BlockedAlert shape rendered by the Telegram formatter.
// Wording mirrors PART 8 of the v9.6 spec:
//
//   - expected_at present + status=expected → "blocked until <expected_at> · <title>"
//   - expected_at present + status=active   → "blocked while active catalyst resolves: <title> (expected <expected_at>)"
//   - expected_at nil + status=expected     → "blocked until catalyst resolution: <title>"
//   - expected_at nil + status=active       → "blocked while active catalyst resolves: <title>"
//
// Resolved/stale/invalidated rows never reach this projector — the
// caller (PrimaryCatalyst) only emits active/expected rows.
func blockedFromCatalyst(c repository.EventCatalyst) *anomaly.BlockedAlert {
	timing := "unknown"
	if !c.ExpectedAt.IsZero() {
		timing = c.ExpectedAt.UTC().Format(time.RFC3339)
	}
	var status string
	switch c.Status {
	case repository.CatalystStatusActive:
		if !c.ExpectedAt.IsZero() {
			status = fmt.Sprintf("blocked while active catalyst resolves: %s (expected %s)", c.Title, timing)
		} else {
			status = "blocked while active catalyst resolves: " + c.Title
		}
	default: // expected
		if !c.ExpectedAt.IsZero() {
			status = fmt.Sprintf("blocked until %s · %s", timing, c.Title)
		} else {
			status = "blocked until catalyst resolution: " + c.Title
		}
	}
	return &anomaly.BlockedAlert{
		Status:               status,
		Reason:               c.Description,
		CatalystType:         string(c.CatalystType),
		ExpectedTiming:       timing,
		BullishScenario:      c.BullishScenario,
		BearishScenario:      c.BearishScenario,
		InvalidationScenario: c.InvalidationScenario,
		Stance:               "accumulation before catalyst is more meaningful than chasing after repricing",
	}
}

// LoadActiveForEventSlug is the slug-keyed variant the postmortem +
// market-intel paths call when they already have the slug.
func (p *Provider) LoadActiveForEventSlug(ctx context.Context, eventSlug string) []repository.EventCatalyst {
	if !p.cfg.Enabled || p.store == nil || eventSlug == "" {
		return nil
	}
	rows, err := p.store.ListActive(ctx, eventSlug)
	if err != nil {
		return nil
	}
	return rows
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
	p.log.Debug().Str("condition_id", conditionID).Msg("event catalyst: unresolved event slug")
}

// --- rendering -----------------------------------------------------------

// RenderCatalystPromptBlock formats the catalyst rows into the
// "Future catalysts:" prompt slot. Stable, machine-friendly key:
// value layout, capped at maxItems + maxChars. Polymarket / AI
// content lands verbatim — Title/Description/Scenarios are DATA.
func RenderCatalystPromptBlock(rows []repository.EventCatalyst, maxItems, maxChars int) string {
	if len(rows) == 0 {
		return ""
	}
	if maxItems <= 0 {
		maxItems = len(rows)
	}
	var b strings.Builder
	for i, c := range rows {
		if i >= maxItems {
			break
		}
		eta := "tbd"
		if !c.ExpectedAt.IsZero() {
			eta = c.ExpectedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "- type=%s | status=%s | expected_at=%s | title=%s\n",
			c.CatalystType, c.Status, eta, oneLine(c.Title))
		if c.Description != "" {
			fmt.Fprintf(&b, "  description: %s\n", compact(c.Description, 360))
		}
		if c.BullishScenario != "" {
			fmt.Fprintf(&b, "  bullish: %s\n", compact(c.BullishScenario, 240))
		}
		if c.BearishScenario != "" {
			fmt.Fprintf(&b, "  bearish: %s\n", compact(c.BearishScenario, 240))
		}
		if c.InvalidationScenario != "" {
			fmt.Fprintf(&b, "  invalidation: %s\n", compact(c.InvalidationScenario, 240))
		}
	}
	out := b.String()
	if maxChars > 0 && len(out) > maxChars {
		out = out[:maxChars-1] + "…"
	}
	return out
}

// PrimaryCatalyst returns the most relevant catalyst for a Telegram
// "Blocked Alert" block: prefers active over expected, then nearest
// expected_at. Returns false when no row qualifies.
func PrimaryCatalyst(rows []repository.EventCatalyst) (repository.EventCatalyst, bool) {
	var best repository.EventCatalyst
	found := false
	for _, c := range rows {
		if !found {
			best = c
			found = true
			continue
		}
		// Active beats expected.
		if best.Status != repository.CatalystStatusActive && c.Status == repository.CatalystStatusActive {
			best = c
			continue
		}
		if best.Status == c.Status {
			// Nearest expected_at wins (NULL last).
			if best.ExpectedAt.IsZero() && !c.ExpectedAt.IsZero() {
				best = c
			} else if !best.ExpectedAt.IsZero() && !c.ExpectedAt.IsZero() && c.ExpectedAt.Before(best.ExpectedAt) {
				best = c
			}
		}
	}
	return best, found
}

// --- finding helpers (mirror eventpagecontext) -----------------------

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
