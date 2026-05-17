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
	// SpanActual is the observed time between the oldest and newest live
	// sample (after window trimming). 0 when fewer than two samples exist.
	// This is the truth that should be displayed in alerts — the configured
	// Window is just the upper bound, not a requirement.
	SpanActual time.Duration
	// OldestAt is the timestamp of the oldest live sample, or zero if empty.
	OldestAt time.Time
}

// Config controls memory and freshness. All valid trades enter the
// reservoir — readiness gates (`SINGLE_MIN_BASELINE_TRADES`,
// `SINGLE_MIN_BASELINE_NOTIONAL_USD`, `BASELINE_MIN_READY_WINDOW`) are
// enforced downstream by the detector, not here.
type Config struct {
	// Window is the maximum sample age. Older samples are dropped on access.
	// 0 means "no upper bound" (only MaxSamples caps memory).
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

// New constructs a Baseline. Window=0 means "no upper bound" — samples are
// only evicted by the per-bucket ring cap (MaxSamples). That is the right
// behaviour for very long-lived markets; readiness is still enforced by the
// detector via MinBaselineTrades, MinBaselineNotionalUSD, and the
// downstream BaselineMinReadySpan gate.
func New(cfg Config) *Baseline {
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = defaultMaxSamples
	}
	if cfg.Window < 0 {
		cfg.Window = 0
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Baseline{cfg: cfg, buckets: make(map[Key]*ring), now: now}
}

// Add records one trade's USD notional in the bucket. Safe for concurrent
// calls. Every positive notional enters the reservoir — there is no
// per-trade size filter. The detector's readiness gates (count, total USD,
// observed span) protect against thin or all-dust baselines.
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
// Empty bucket → zero Stats. When cfg.Window == 0 the trim step is skipped —
// the ring is the only bound.
func (b *Baseline) Stats(k Key) Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.buckets[k]
	if !ok {
		return Stats{}
	}
	if b.cfg.Window > 0 {
		r.trim(b.now().Add(-b.cfg.Window))
	}
	if r.size == 0 {
		return Stats{}
	}
	notionals := make([]float64, 0, r.size)
	cap := len(r.buf)
	var oldest, newest time.Time
	for i := 0; i < r.size; i++ {
		s := r.buf[(r.head+i)%cap]
		notionals = append(notionals, s.notional)
		if i == 0 || s.at.Before(oldest) {
			oldest = s.at
		}
		if i == 0 || s.at.After(newest) {
			newest = s.at
		}
	}
	sort.Float64s(notionals)
	mean := r.sum / float64(r.size)
	return Stats{
		Count:      r.size,
		MeanUSD:    mean,
		MedianUSD:  percentile(notionals, 0.5),
		P95USD:     percentile(notionals, 0.95),
		TotalUSD:   r.sum,
		SpanActual: newest.Sub(oldest),
		OldestAt:   oldest,
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
