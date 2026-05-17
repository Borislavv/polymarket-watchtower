// Package ratelimit is a thin wrapper around x/time/rate that pairs a
// token-bucket with the per-host configuration so callers don't need to know
// the per-second / burst numbers.
package ratelimit

import (
	"context"

	"golang.org/x/time/rate"
)

// Limiter is the small surface use-cases need. We don't re-expose Wait variants
// — the only caller pattern is "block until allowed, respecting ctx".
type Limiter interface {
	Wait(ctx context.Context) error
}

// New returns a token-bucket limiter with rps tokens/second and the given burst.
func New(rps float64, burst int) Limiter {
	if rps <= 0 {
		rps = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}

// Noop is a limiter that never blocks. Useful for tests.
type Noop struct{}

func (Noop) Wait(context.Context) error { return nil }
