// Package cluster watches per-category sliding windows of anomalous trades and
// emits a HARD alert (CategoryWatchRequired) when N anomalous trades from M
// unique wallets totalling >= X USD land in one category within a configurable
// window. This is the "many sharks circling one category" signal — the highest-
// severity output of the watchtower.
package cluster

import (
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Config defines the trigger and dedup behavior.
type Config struct {
	Window           time.Duration
	MinTrades        int
	MinUniqueWallets int
	MinTotalUSD      float64
	// Cooldown between consecutive alerts on the same category.
	Cooldown time.Duration
	// SampleCap caps the number of contributing trades carried in the alert
	// payload (only for telegram/log context; full set is in metrics).
	SampleCap int
	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time
}

const defaultSampleCap = 5

// Detector is concurrency-safe.
type Detector struct {
	cfg      Config
	now      func() time.Time
	mu       sync.Mutex
	perCat   map[vo.CategoryID]*window
	lastFire map[vo.CategoryID]time.Time
}

type entry struct {
	at       time.Time
	wallet   string
	notional float64
	trade    anomaly.TradeRef
}

type window struct {
	entries []entry
}

func New(cfg Config) *Detector {
	if cfg.Window <= 0 {
		cfg.Window = time.Hour
	}
	if cfg.MinTrades <= 0 {
		cfg.MinTrades = 5
	}
	if cfg.MinUniqueWallets <= 0 {
		cfg.MinUniqueWallets = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = cfg.Window // by default, one alert per window per category
	}
	if cfg.SampleCap <= 0 {
		cfg.SampleCap = defaultSampleCap
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Detector{
		cfg:      cfg,
		now:      now,
		perCat:   make(map[vo.CategoryID]*window),
		lastFire: make(map[vo.CategoryID]time.Time),
	}
}

// Observe records one anomalous trade in the given category and returns a
// non-nil Stats when the cluster criteria are met and the cooldown allows
// firing. Caller is responsible for building the Finding envelope.
func (d *Detector) Observe(cat vo.CategoryID, tr anomaly.TradeRef) *anomaly.ClusterStats {
	now := d.now()
	cutoff := now.Add(-d.cfg.Window)

	d.mu.Lock()
	defer d.mu.Unlock()

	w := d.perCat[cat]
	if w == nil {
		w = &window{}
		d.perCat[cat] = w
	}
	// Drop expired entries (cheap because windows are usually small).
	w.entries = trimBefore(w.entries, cutoff)
	w.entries = append(w.entries, entry{
		at: tr.At, wallet: tr.Wallet, notional: tr.NotionalUSD, trade: tr,
	})

	// Quick fail before computing the expensive bits.
	if len(w.entries) < d.cfg.MinTrades {
		return nil
	}
	wallets := uniqueWallets(w.entries)
	if wallets < d.cfg.MinUniqueWallets {
		return nil
	}
	total := totalUSD(w.entries)
	if total < d.cfg.MinTotalUSD {
		return nil
	}
	// Cooldown gate.
	if last, ok := d.lastFire[cat]; ok && now.Sub(last) < d.cfg.Cooldown {
		return nil
	}
	d.lastFire[cat] = now

	return &anomaly.ClusterStats{
		Window:          d.cfg.Window,
		AnomalousTrades: len(w.entries),
		UniqueWallets:   wallets,
		TotalUSD:        total,
		Sample:          headSample(w.entries, d.cfg.SampleCap),
	}
}

// Count returns the number of entries currently in the per-category window.
// Used by single-trade alerts to render the "part of N-trade cluster" hint
// without firing a cluster alert.
func (d *Detector) Count(cat vo.CategoryID) int {
	cutoff := d.now().Add(-d.cfg.Window)
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.perCat[cat]
	if w == nil {
		return 0
	}
	w.entries = trimBefore(w.entries, cutoff)
	return len(w.entries)
}

// Forget releases the per-category state. Useful when discovery prunes a
// category entirely (rare).
func (d *Detector) Forget(cat vo.CategoryID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.perCat, cat)
	delete(d.lastFire, cat)
}

func trimBefore(es []entry, cutoff time.Time) []entry {
	i := 0
	for i < len(es) && es[i].at.Before(cutoff) {
		i++
	}
	if i == 0 {
		return es
	}
	// shift left to keep allocation churn down
	return append(es[:0], es[i:]...)
}

func uniqueWallets(es []entry) int {
	if len(es) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(es))
	for _, e := range es {
		if e.wallet == "" {
			continue
		}
		seen[e.wallet] = struct{}{}
	}
	return len(seen)
}

func totalUSD(es []entry) float64 {
	var sum float64
	for _, e := range es {
		sum += e.notional
	}
	return sum
}

func headSample(es []entry, cap int) []anomaly.TradeRef {
	if cap <= 0 || cap > len(es) {
		cap = len(es)
	}
	out := make([]anomaly.TradeRef, cap)
	for i := 0; i < cap; i++ {
		out[i] = es[i].trade
	}
	return out
}
