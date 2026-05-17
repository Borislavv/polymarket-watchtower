package trade

import (
	"math"
	"sort"
	"time"
)

// Bucket is a fixed-width time-bucketed aggregate of trades.
type Bucket struct {
	Start    time.Time
	Count    int64
	Notional float64
	SizeSum  float64
	SizeSqr  float64
	SizeMin  float64
	SizeMax  float64
	sizes    []float64 // retained for percentile/median calculations
}

// Add folds a trade into the bucket. Sizes are retained until Snapshot is
// called so we can compute medians cheaply on demand.
func (b *Bucket) Add(t Trade) {
	if b.Count == 0 {
		b.SizeMin = t.Size
		b.SizeMax = t.Size
	} else {
		if t.Size < b.SizeMin {
			b.SizeMin = t.Size
		}
		if t.Size > b.SizeMax {
			b.SizeMax = t.Size
		}
	}
	b.Count++
	b.Notional += t.NotionalUSD()
	b.SizeSum += t.Size
	b.SizeSqr += t.Size * t.Size
	b.sizes = append(b.sizes, t.Size)
}

// Window is a read-only aggregate over a [Start, End) span. It is the unit the
// detector consumes when comparing baseline vs recent windows.
type Window struct {
	Start, End time.Time
	Count      int64
	Notional   float64
	SizeSum    float64
	SizeMin    float64
	SizeMax    float64
	SizeMedian float64
}

// Duration returns End - Start.
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// AvgSize returns SizeSum / Count (or 0 when empty).
func (w Window) AvgSize() float64 {
	if w.Count == 0 {
		return 0
	}
	return w.SizeSum / float64(w.Count)
}

// TradesPerMinute normalises Count over the window length.
func (w Window) TradesPerMinute() float64 {
	mins := w.Duration().Minutes()
	if mins <= 0 {
		return 0
	}
	return float64(w.Count) / mins
}

// NotionalPerMinute normalises Notional over the window length.
func (w Window) NotionalPerMinute() float64 {
	mins := w.Duration().Minutes()
	if mins <= 0 {
		return 0
	}
	return w.Notional / mins
}

// FoldBuckets reduces a slice of Buckets into a single Window covering
// [start, end). Buckets entirely outside the range are skipped.
func FoldBuckets(buckets []Bucket, start, end time.Time) Window {
	w := Window{Start: start, End: end}
	var sizes []float64
	for i := range buckets {
		b := &buckets[i]
		if b.Count == 0 {
			continue
		}
		if b.Start.Before(start) || !b.Start.Before(end) {
			continue
		}
		w.Count += b.Count
		w.Notional += b.Notional
		w.SizeSum += b.SizeSum
		if len(sizes) == 0 {
			w.SizeMin = b.SizeMin
			w.SizeMax = b.SizeMax
		} else {
			if b.SizeMin < w.SizeMin {
				w.SizeMin = b.SizeMin
			}
			if b.SizeMax > w.SizeMax {
				w.SizeMax = b.SizeMax
			}
		}
		sizes = append(sizes, b.sizes...)
	}
	if len(sizes) > 0 {
		sort.Float64s(sizes)
		mid := len(sizes) / 2
		if len(sizes)%2 == 0 {
			w.SizeMedian = (sizes[mid-1] + sizes[mid]) / 2
		} else {
			w.SizeMedian = sizes[mid]
		}
	}
	if math.IsNaN(w.Notional) {
		w.Notional = 0
	}
	return w
}
