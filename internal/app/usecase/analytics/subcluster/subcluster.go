// Package subcluster watches per-category sliding windows of *sub-threshold*
// trades — trades that don't clear the single-trade absolute floor but still
// share the shape of an insider bet (decent USD, asymmetric odds, far above
// the baseline median). When enough distinct wallets accumulate enough total
// USD inside the window, the detector fires a HARD "WhaleClusterDetected"
// alert: this is the signal that a split-wallet insider strategy is in play.
//
// Why a separate package from cluster:
//
//   - cluster only sees trades that already fired a single-trade alert.
//   - subcluster sees trades that did *not* fire one (below absolute floor).
//   - Both can therefore fire on the same window without double-counting.
//
// All state mutation is concurrency-safe.
package subcluster

import (
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Config defines per-candidate qualification and the cluster trigger.
type Config struct {
	// Per-candidate gates — a trade must clear ALL three to be admitted.
	MinTradeUSD   float64
	MinOdds       float64
	MinMultiplier float64

	// Window over which candidates accumulate (per category).
	Window time.Duration

	// Trigger.
	MinUniqueWallets    int
	MinTotalNotionalUSD float64

	// Cooldown between fires on the same category.
	Cooldown time.Duration

	// SampleCap bounds the contributing-trade sample copied into the alert.
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

// New constructs a Detector. Zero-valued fields get conservative defaults so a
// misconfigured detector is hard to trigger rather than easy to spam.
func New(cfg Config) *Detector {
	if cfg.Window <= 0 {
		cfg.Window = time.Hour
	}
	if cfg.MinUniqueWallets <= 0 {
		cfg.MinUniqueWallets = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = cfg.Window
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

// Qualifies reports whether the (notional, odds, multiplier) triple clears the
// per-candidate floors. Exposed so callers can short-circuit without needing
// to take the detector's lock when the trade clearly fails.
func (d *Detector) Qualifies(notional, odds, multiplier float64) bool {
	if d.cfg.MinTradeUSD > 0 && notional < d.cfg.MinTradeUSD {
		return false
	}
	if d.cfg.MinOdds > 0 && odds < d.cfg.MinOdds {
		return false
	}
	if d.cfg.MinMultiplier > 0 && multiplier < d.cfg.MinMultiplier {
		return false
	}
	return true
}

// Observe admits the trade if it qualifies and returns non-nil Stats when the
// window crosses the trigger and the cooldown allows firing.
func (d *Detector) Observe(cat vo.CategoryID, tr anomaly.TradeRef, odds, multiplier float64) *anomaly.ClusterStats {
	if !d.Qualifies(tr.NotionalUSD, odds, multiplier) {
		return nil
	}

	now := d.now()
	cutoff := now.Add(-d.cfg.Window)

	d.mu.Lock()
	defer d.mu.Unlock()

	w := d.perCat[cat]
	if w == nil {
		w = &window{}
		d.perCat[cat] = w
	}
	w.entries = trimBefore(w.entries, cutoff)
	w.entries = append(w.entries, entry{
		at: tr.At, wallet: tr.Wallet, notional: tr.NotionalUSD, trade: tr,
	})

	wallets := uniqueWallets(w.entries)
	if wallets < d.cfg.MinUniqueWallets {
		return nil
	}
	total := totalUSD(w.entries)
	if total < d.cfg.MinTotalNotionalUSD {
		return nil
	}
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

// Count returns the number of admitted entries currently in the per-category
// window. Useful for diagnostics.
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

// Forget releases per-category state.
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
