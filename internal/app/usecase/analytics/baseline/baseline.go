// Package baseline keeps a rolling per-bucket reservoir of trade USD notionals
// and exposes summary statistics (median, mean, p95, count) used by the
// per-trade anomaly scorer.
//
// A bucket is (category, market, outcome-token). Samples are kept in a capped
// ring buffer trimmed by age on read — bounded memory regardless of upstream
// trade volume. Median/p95 are computed by sort on demand; with the default
// cap of 1024 samples per bucket this is negligible relative to network I/O.
package baseline

import (
	"sort"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Key identifies a baseline bucket. OutcomeToken is the trade's asset id.
type Key struct {
	Category     vo.CategoryID
	Market       vo.MarketID
	OutcomeToken vo.TokenID
}

// Stats is a read-only summary of a bucket's recent samples.
type Stats struct {
	Count     int
	MeanUSD   float64
	MedianUSD float64
	P95USD    float64
	TotalUSD  float64
}

// Config controls memory and freshness.
type Config struct {
	// Window is the maximum sample age. Older samples are dropped on access.
	Window time.Duration
	// MaxSamples caps the ring buffer per bucket. 0 → defaultMaxSamples.
	MaxSamples int
	// Clock optionally overrides time.Now (tests).
	Clock func() time.Time
}

const defaultMaxSamples = 1024

// Baseline is concurrency-safe.
type Baseline struct {
	mu      sync.RWMutex
	cfg     Config
	buckets map[Key]*ring
	now     func() time.Time
}

type sample struct {
	notional float64
	at       time.Time
}

type ring struct {
	buf  []sample
	head int // index of oldest sample
	size int // count of live samples
	sum  float64
}

func New(cfg Config) *Baseline {
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = defaultMaxSamples
	}
	if cfg.Window <= 0 {
		cfg.Window = 7 * 24 * time.Hour
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Baseline{cfg: cfg, buckets: make(map[Key]*ring), now: now}
}

// Add records one trade's USD notional in the bucket. Safe for concurrent calls.
func (b *Baseline) Add(k Key, notionalUSD float64, at time.Time) {
	if notionalUSD <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.buckets[k]
	if !ok {
		r = &ring{buf: make([]sample, b.cfg.MaxSamples)}
		b.buckets[k] = r
	}
	r.push(sample{notional: notionalUSD, at: at})
}

// Stats returns a snapshot for the bucket after dropping out-of-window samples.
// Empty bucket → zero Stats.
func (b *Baseline) Stats(k Key) Stats {
	cutoff := b.now().Add(-b.cfg.Window)
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.buckets[k]
	if !ok {
		return Stats{}
	}
	r.trim(cutoff)
	if r.size == 0 {
		return Stats{}
	}
	notionals := r.snapshot()
	sort.Float64s(notionals)
	mean := r.sum / float64(r.size)
	return Stats{
		Count:     r.size,
		MeanUSD:   mean,
		MedianUSD: percentile(notionals, 0.5),
		P95USD:    percentile(notionals, 0.95),
		TotalUSD:  r.sum,
	}
}

// Buckets returns the number of live buckets; useful for diagnostics + metrics.
func (b *Baseline) Buckets() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.buckets)
}

func (r *ring) push(s sample) {
	cap := len(r.buf)
	if r.size < cap {
		idx := (r.head + r.size) % cap
		r.buf[idx] = s
		r.size++
		r.sum += s.notional
		return
	}
	// full — overwrite oldest
	r.sum -= r.buf[r.head].notional
	r.buf[r.head] = s
	r.head = (r.head + 1) % cap
	r.sum += s.notional
}

// trim drops samples older than cutoff from the head.
func (r *ring) trim(cutoff time.Time) {
	cap := len(r.buf)
	for r.size > 0 {
		s := r.buf[r.head]
		if !s.at.Before(cutoff) {
			return
		}
		r.sum -= s.notional
		r.head = (r.head + 1) % cap
		r.size--
	}
}

func (r *ring) snapshot() []float64 {
	out := make([]float64, r.size)
	cap := len(r.buf)
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(r.head+i)%cap].notional
	}
	return out
}

// percentile assumes sorted asc. p in [0,1].
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	// nearest-rank, 1-based; clamp to last index.
	rank := int(float64(len(sorted))*p + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
