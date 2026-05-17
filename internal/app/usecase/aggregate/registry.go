package aggregate

import (
	"sync"

	market2 "github.com/Borislavv/polymarket-watchtower/internal/domain/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// MarketRegistry remembers the live set of markets discovered from Gamma and
// their categorization. It is consumed by every loop that needs to iterate or
// label markets.
type MarketRegistry struct {
	mu         sync.RWMutex
	markets    map[vo.MarketID]market2.Market
	categories map[vo.CategoryID]market2.Category
}

func NewRegistry() *MarketRegistry {
	return &MarketRegistry{
		markets:    make(map[vo.MarketID]market2.Market),
		categories: make(map[vo.CategoryID]market2.Category),
	}
}

// Replace swaps in a new market+category set and returns the IDs that were
// present before but are no longer present. The caller is expected to use the
// returned slice to release downstream state (e.g. engine bucket rings).
func (r *MarketRegistry) Replace(markets []market2.Market, cats []market2.Category) []vo.MarketID {
	r.mu.Lock()
	defer r.mu.Unlock()
	nextMarkets := make(map[vo.MarketID]market2.Market, len(markets))
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
	r.categories = make(map[vo.CategoryID]market2.Category, len(cats))
	for _, c := range cats {
		r.categories[c.ID] = c
	}
	return removed
}

func (r *MarketRegistry) Snapshot() []market2.Market {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]market2.Market, 0, len(r.markets))
	for _, m := range r.markets {
		out = append(out, m)
	}
	return out
}

func (r *MarketRegistry) Get(id vo.MarketID) (market2.Market, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.markets[id]
	return m, ok
}

func (r *MarketRegistry) Category(id vo.CategoryID) (market2.Category, bool) {
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
