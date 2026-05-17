package aggregate

import (
	"sync"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/market"
	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/vo"
)

// MarketRegistry remembers the live set of markets discovered from Gamma and
// their categorization. It is consumed by every loop that needs to iterate or
// label markets.
type MarketRegistry struct {
	mu         sync.RWMutex
	markets    map[vo.MarketID]market.Market
	categories map[vo.CategoryID]market.Category
}

func NewRegistry() *MarketRegistry {
	return &MarketRegistry{
		markets:    make(map[vo.MarketID]market.Market),
		categories: make(map[vo.CategoryID]market.Category),
	}
}

// Replace swaps in a new market+category set and returns the IDs that were
// present before but are no longer present. The caller is expected to use the
// returned slice to release downstream state (e.g. engine bucket rings).
func (r *MarketRegistry) Replace(markets []market.Market, cats []market.Category) []vo.MarketID {
	r.mu.Lock()
	defer r.mu.Unlock()
	nextMarkets := make(map[vo.MarketID]market.Market, len(markets))
	for _, m := range markets {
		nextMarkets[m.ID] = m
	}
	var removed []vo.MarketID
	for id := range r.markets {
		if _, kept := nextMarkets[id]; !kept {
			removed = append(removed, id)
		}
	}
	r.markets = nextMarkets
	r.categories = make(map[vo.CategoryID]market.Category, len(cats))
	for _, c := range cats {
		r.categories[c.ID] = c
	}
	return removed
}

func (r *MarketRegistry) Snapshot() []market.Market {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]market.Market, 0, len(r.markets))
	for _, m := range r.markets {
		out = append(out, m)
	}
	return out
}

func (r *MarketRegistry) Get(id vo.MarketID) (market.Market, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.markets[id]
	return m, ok
}

func (r *MarketRegistry) Category(id vo.CategoryID) (market.Category, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.categories[id]
	return c, ok
}

func (r *MarketRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.markets)
}
