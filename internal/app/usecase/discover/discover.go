// Package discover periodically pulls the market universe and category
// list from Gamma and refreshes the in-memory marketcache. The DB write-
// through (persist.Sink) is the source of truth; the cache is the
// per-trade hot-path accelerator that detect.Loop consumes for category
// label lookups.
package discover

import (
	"context"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/rs/zerolog"
)

// Persist is the optional Postgres side-channel. When set the loop hands
// the filtered category + market sets to the sink after the cache
// refresh. Errors are logged but never fail the tick — the cache stays
// usable.
type Persist func(ctx context.Context, cats []market.Category, markets []market.Market) error

type Config struct {
	Interval time.Duration
	// SafetyMaxMarkets is an OPERATIONAL EMERGENCY CAP on rows fetched
	// per sweep (Gamma `MaxRows`). 0 means unlimited and is the only
	// correct setting for normal production. See PipelineConfig docs.
	SafetyMaxMarkets int
	ActiveOnly       bool
	OrderBy          string
	// Persist receives every discovered (categories, markets) pair for
	// write-through to PostgreSQL. Nil = no DB writes (dev only).
	Persist Persist
}

type Loop struct {
	cfg     Config
	client  *gamma.Client
	cache   *marketcache.Cache
	filter  *category.Filter
	metrics *metrics.Metrics
	log     *zerolog.Logger
}

func New(
	cfg Config,
	c *gamma.Client,
	cache *marketcache.Cache,
	filter *category.Filter,
	m *metrics.Metrics,
	log *zerolog.Logger,
) *Loop {
	if filter == nil {
		filter = category.NewFilter(nil)
	}
	return &Loop{cfg: cfg, client: c, cache: cache, filter: filter, metrics: m, log: log}
}

// Run executes one fetch immediately and then on every Interval until ctx
// is cancelled. Transient errors are logged; the loop continues.
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
		MaxRows:    l.cfg.SafetyMaxMarkets,
	})
	if err != nil {
		return err
	}
	tagsByCondition := gamma.MapEventsToMarketCategories(events)

	markets, err := l.client.ListMarkets(ctx, gamma.ListMarketsOpts{
		ActiveOnly: l.cfg.ActiveOnly,
		OrderBy:    l.cfg.OrderBy,
		MaxRows:    l.cfg.SafetyMaxMarkets,
	})
	if err != nil {
		return err
	}

	// Build the set of category ids blocked by the filter, walking the
	// freshly-fetched /tags response. Subsequent loops apply this set to
	// each market's Categories and to the cats slice itself so the cache
	// never sees them.
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

	// Refresh the cache; the returned set of removed ids is intentionally
	// unused here — discover never owned per-market state beyond the
	// cache itself, and downstream packages (cluster windows, sender
	// queue) are bounded by their own retention rules.
	_ = l.cache.Replace(markets, cats)
	l.metrics.MarketsTracked.Set(float64(l.cache.Size()))
	l.log.Info().
		Int("markets", len(markets)).
		Int("categories", len(cats)).
		Int("filtered_assignments", skippedAssignments).
		Int("filtered_categories", len(denied)).
		Msg("discover: refreshed")

	if l.cfg.Persist != nil {
		if err := l.cfg.Persist(ctx, cats, markets); err != nil {
			// DB write failures are operational, not fatal — alerting
			// keeps working off the in-memory cache.
			l.log.Err(err).Msg("discover: persist failed")
		}
	}
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

// Replace seeds the cache without going through the HTTP client. Used by
// tests that need a deterministic universe.
func (l *Loop) Replace(markets []market.Market, cats []market.Category) {
	_ = l.cache.Replace(markets, cats)
}

// RunOnce performs one discovery tick; exposed for tests.
func (l *Loop) RunOnce(ctx context.Context) error { return l.tick(ctx) }
