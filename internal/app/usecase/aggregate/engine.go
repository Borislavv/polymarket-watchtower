// Package aggregate is the in-memory rolling-bucket engine. It is the single
// home of "what we know about a market right now" and the only place trades
// mutate.
package aggregate

import (
	"sync"
	"time"

	trade2 "github.com/Borislavv/polymarket-watchtower/internal/domain/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Engine holds a ring of per-bucket trade aggregates per market.
type Engine struct {
	mu             sync.RWMutex
	bucket         time.Duration
	baseline       time.Duration
	bucketsPerRing int
	clock          func() time.Time
	rings          map[vo.MarketID]*marketRing
}

// Config sizes the rolling window. baseline must be >= every recent window
// the caller plans to fold.
type Config struct {
	Bucket   time.Duration
	Baseline time.Duration
	Clock    func() time.Time // optional override for tests
}

// New constructs an Engine.
func New(cfg Config) *Engine {
	if cfg.Bucket <= 0 {
		cfg.Bucket = time.Minute
	}
	if cfg.Baseline <= 0 {
		cfg.Baseline = 7 * 24 * time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	n := int(cfg.Baseline / cfg.Bucket)
	if n < 2 {
		n = 2
	}
	return &Engine{
		bucket:         cfg.Bucket,
		baseline:       cfg.Baseline,
		bucketsPerRing: n,
		clock:          cfg.Clock,
		rings:          make(map[vo.MarketID]*marketRing),
	}
}

// marketRing is a fixed-size circular buffer keyed by bucket start.
type marketRing struct {
	buckets []trade2.Bucket
	size    int
}

func (e *Engine) ringFor(id vo.MarketID) *marketRing {
	r, ok := e.rings[id]
	if !ok {
		r = &marketRing{buckets: make([]trade2.Bucket, e.bucketsPerRing), size: e.bucketsPerRing}
		e.rings[id] = r
	}
	return r
}

func (e *Engine) bucketStart(t time.Time) time.Time {
	return t.Truncate(e.bucket)
}

// Ingest folds a trade into the right bucket. Trades older than the baseline
// window are dropped silently.
func (e *Engine) Ingest(t trade2.Trade) {
	if t.Market == "" {
		return
	}
	now := e.clock()
	bs := e.bucketStart(t.Timestamp)
	if now.Sub(bs) >= e.baseline {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	r := e.ringFor(t.Market)
	idx := int(bs.UnixNano()/int64(e.bucket)) % r.size
	if idx < 0 {
		idx += r.size
	}
	b := &r.buckets[idx]
	if !b.Start.Equal(bs) {
		// stale slot — reset
		*b = trade2.Bucket{Start: bs}
	}
	b.Add(t)
}

// IngestBatch is Ingest applied across a slice.
func (e *Engine) IngestBatch(ts []trade2.Trade) {
	for _, t := range ts {
		e.Ingest(t)
	}
}

// Window folds the current ring for market `id` into a single Window covering
// the last `length` of time ending at now.
func (e *Engine) Window(id vo.MarketID, length time.Duration) trade2.Window {
	now := e.clock()
	start := now.Add(-length)
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.rings[id]
	if !ok {
		return trade2.Window{Start: start, End: now}
	}
	live := make([]trade2.Bucket, 0, r.size)
	for i := range r.buckets {
		b := r.buckets[i]
		if b.Count == 0 {
			continue
		}
		live = append(live, b)
	}
	return trade2.FoldBuckets(live, start, now)
}

// BaselineWindow returns the full baseline window (excluding the most recent
// `exclude` time, so a recent-vs-baseline comparison is not self-overlapping).
func (e *Engine) BaselineWindow(id vo.MarketID, exclude time.Duration) trade2.Window {
	now := e.clock()
	end := now.Add(-exclude)
	if end.Before(now.Add(-e.baseline)) {
		end = now.Add(-e.baseline)
	}
	start := now.Add(-e.baseline)
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.rings[id]
	if !ok {
		return trade2.Window{Start: start, End: end}
	}
	live := make([]trade2.Bucket, 0, r.size)
	for i := range r.buckets {
		b := r.buckets[i]
		if b.Count == 0 {
			continue
		}
		live = append(live, b)
	}
	return trade2.FoldBuckets(live, start, end)
}

// Markets returns the set of market IDs currently tracked.
func (e *Engine) Markets() []vo.MarketID {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]vo.MarketID, 0, len(e.rings))
	for id := range e.rings {
		out = append(out, id)
	}
	return out
}

// Forget releases the ring for the given market (used when discovery prunes it).
func (e *Engine) Forget(id vo.MarketID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.rings, id)
}

// BaselineWindowLen exposes the configured baseline.
func (e *Engine) BaselineWindowLen() time.Duration { return e.baseline }
