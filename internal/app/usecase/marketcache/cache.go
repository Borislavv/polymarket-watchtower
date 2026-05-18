// Package marketcache is the read-only in-memory snapshot of the active
// market universe + the category lookup table. It is a CACHE — Postgres
// is the source of truth, and every cache miss must fall back to the
// repository.
//
// Why a cache at all: the per-trade detector calls Category(id) on the
// hot path to resolve a category id to its human label. A DB hop per
// trade would be wasteful when discover already has the answer.
//
// Contract:
//   - discover.Loop is the single writer; it calls Replace on every
//     successful sweep AFTER the row has been persisted by persist.Sink.
//   - Reads are concurrency-safe.
//   - Snapshot returns a defensive copy; callers do not need to lock.
//   - Get / Category return (value, false) on miss; callers fall back to
//     the repository when they need a guaranteed answer.
//   - Replace returns the set of market ids that disappeared between the
//     previous snapshot and the new one, so callers can release any
//     per-market state they own.
package marketcache

import (
	"sync"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Cache is the in-memory snapshot. Zero value is NOT usable; use New.
type Cache struct {
	mu         sync.RWMutex
	markets    map[vo.MarketID]market.Market
	categories map[vo.CategoryID]market.Category
}

// New constructs an empty Cache.
func New() *Cache {
	return &Cache{
		markets:    make(map[vo.MarketID]market.Market),
		categories: make(map[vo.CategoryID]market.Category),
	}
}

// Replace swaps in a fresh market + category set. Returns the market ids
// present before but missing now, so callers can release per-market
// state (e.g. cluster windows referencing a market that has ended).
func (c *Cache) Replace(markets []market.Market, cats []market.Category) []vo.MarketID {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[vo.MarketID]market.Market, len(markets))
	for _, m := range markets {
		next[m.ID] = m
	}
	var removed []vo.MarketID
	for id := range c.markets {
		if _, kept := next[id]; !kept {
			removed = append(removed, id)
		}
	}
	c.markets = next
	c.categories = make(map[vo.CategoryID]market.Category, len(cats))
	for _, ct := range cats {
		c.categories[ct.ID] = ct
	}
	return removed
}

// Snapshot returns a defensive copy of the current market set. Safe to
// iterate without holding the cache lock.
func (c *Cache) Snapshot() []market.Market {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]market.Market, 0, len(c.markets))
	for _, m := range c.markets {
		out = append(out, m)
	}
	return out
}

// Get looks up a market by condition id. Cache miss → (zero, false).
// Callers that need a guaranteed answer fall back to
// repository.MarketRepository.GetByConditionID.
func (c *Cache) Get(id vo.MarketID) (market.Market, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.markets[id]
	return m, ok
}

// Category looks up a category by id. Cache miss → (zero, false).
func (c *Cache) Category(id vo.CategoryID) (market.Category, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ct, ok := c.categories[id]
	return ct, ok
}

// Size reports the number of markets currently cached. Used by the
// `watchtower_discover_markets_tracked` gauge.
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.markets)
}
