// Package discover periodically pulls the market universe and category list
// from Gamma and updates the shared registry.
package discover

import (
	"context"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	market2 "github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/rs/zerolog"
)

type Config struct {
	Interval   time.Duration
	MaxMarkets int
	ActiveOnly bool
	OrderBy    string
}

// Forgetter is the subset of aggregate.Engine that discover relies on to
// release downstream state for markets that disappeared from upstream.
type Forgetter interface {
	Forget(vo.MarketID)
}

type Loop struct {
	cfg      Config
	client   *gamma.Client
	registry *aggregate.MarketRegistry
	engine   Forgetter
	filter   *category.Filter
	metrics  *metrics.Metrics
	log      *zerolog.Logger
}

func New(
	cfg Config,
	c *gamma.Client,
	r *aggregate.MarketRegistry,
	engine Forgetter,
	filter *category.Filter,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
	if filter == nil {
		filter = category.NewFilter(nil)
	}
	return &Loop{cfg: cfg, client: c, registry: r, engine: engine, filter: filter, metrics: m, log: log}
}

// Run executes one fetch immediately and then on every Interval until ctx is
// cancelled. Transient errors are logged; the loop continues.
func (l *Loop) Run(ctx context.Context) error {
	t := time.NewTicker(l.cfg.Interval)
	defer t.Stop()

	if err := l.tick(ctx); err != nil {
		l.log.Err(err).Msg("discover: initial tick failed")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := l.tick(ctx); err != nil {
				l.log.Err(err).Msg("discover: tick failed")
			}
		}
	}
}

func (l *Loop) tick(ctx context.Context) error {
	cats, err := l.client.ListTags(ctx)
	if err != nil {
		return err
	}
	events, err := l.client.ListEvents(ctx, gamma.ListMarketsOpts{
		ActiveOnly: l.cfg.ActiveOnly,
		OrderBy:    l.cfg.OrderBy,
		MaxRows:    l.cfg.MaxMarkets,
	})
	if err != nil {
		return err
	}
	tagsByCondition := gamma.MapEventsToMarketCategories(events)

	markets, err := l.client.ListMarkets(ctx, gamma.ListMarketsOpts{
		ActiveOnly: l.cfg.ActiveOnly,
		OrderBy:    l.cfg.OrderBy,
		MaxRows:    l.cfg.MaxMarkets,
	})
	if err != nil {
		return err
	}

	// Build the set of category ids blocked by the filter, walking the freshly-
	// fetched /tags response. Subsequent loops apply this set to each market's
	// Categories and to the cats slice itself so the registry never sees them.
	denied := make(map[vo.CategoryID]struct{}, len(cats))
	keptCats := cats[:0]
	for _, c := range cats {
		if l.filter.Allowed(c.Slug, c.Label) {
			keptCats = append(keptCats, c)
			continue
		}
		denied[c.ID] = struct{}{}
	}
	cats = keptCats

	var skippedAssignments int
	for i := range markets {
		if ids, ok := tagsByCondition[string(markets[i].ID)]; ok {
			ids = dedup(ids)
			kept := ids[:0]
			for _, id := range ids {
				if _, blocked := denied[id]; blocked {
					skippedAssignments++
					continue
				}
				kept = append(kept, id)
			}
			markets[i].Categories = kept
		}
	}
	if skippedAssignments > 0 && l.metrics != nil {
		l.metrics.CategoryFilterSkipped.WithLabelValues("discover").Add(float64(skippedAssignments))
	}

	removed := l.registry.Replace(markets, cats)
	for _, id := range removed {
		if l.engine != nil {
			l.engine.Forget(id)
		}
	}
	l.metrics.MarketsTracked.Set(float64(len(markets)))
	l.log.Info().
		Int("markets", len(markets)).
		Int("categories", len(cats)).
		Int("dropped", len(removed)).
		Int("filtered_assignments", skippedAssignments).
		Int("filtered_categories", len(denied)).
		Msg("discover: refreshed")
	return nil
}

func dedup[T comparable](in []T) []T {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[T]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// Replace allows tests to inject a single round-trip without going through
// the HTTP client.
func (l *Loop) Replace(markets []market2.Market, cats []market2.Category) {
	_ = l.registry.Replace(markets, cats)
}

// RunOnce performs one discovery tick; exposed for tests that need
// deterministic execution without the ticker.
func (l *Loop) RunOnce(ctx context.Context) error { return l.tick(ctx) }
