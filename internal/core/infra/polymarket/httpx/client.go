// Package httpx is a tiny shared HTTP client wrapper used by every Polymarket
// adapter. It enforces:
//   - context cancellation
//   - JSON decoding with strict error mapping
//   - rate-limit gating per host
//   - exponential backoff with jitter on 429 / 5xx
//   - a uniform User-Agent header
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/infra/ratelimit"
	"github.com/rs/zerolog"
)

// ErrNotFound is returned for 404 responses so callers can distinguish missing
// resources from transport failures.
var ErrNotFound = errors.New("httpx: not found")

// ObserveFn is the callback signature for per-request telemetry.
// It is invoked once per round-trip (including failed ones).
type ObserveFn func(endpoint string, status int, duration time.Duration)

// Client is a reusable JSON HTTP client.
type Client struct {
	base      *url.URL
	http      *http.Client
	limiter   ratelimit.Limiter
	userAgent string
	log       *zerolog.Logger
	maxRetry  int
	observe   ObserveFn
}

// Config is the constructor arg for New.
type Config struct {
	BaseURL   string
	Timeout   time.Duration
	UserAgent string
	Limiter   ratelimit.Limiter
	Logger    *zerolog.Logger
	MaxRetry  int
	Observe   ObserveFn
}

func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("httpx: base url required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("httpx: parse base url: %w", err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxRetry == 0 {
		cfg.MaxRetry = 4
	}
	if cfg.Limiter == nil {
		cfg.Limiter = ratelimit.Noop{}
	}
	return &Client{
		base:      u,
		http:      &http.Client{Timeout: cfg.Timeout},
		limiter:   cfg.Limiter,
		userAgent: cfg.UserAgent,
		log:       cfg.Logger,
		maxRetry:  cfg.MaxRetry,
		observe:   cfg.Observe,
	}, nil
}

// GetJSON issues GET path?query and decodes the body into out. Retries on 429
// and 5xx with exponential backoff + jitter.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := *c.base
	u.Path = joinPath(u.Path, path)
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetry; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return fmt.Errorf("httpx: build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}

		start := time.Now()
		resp, err := c.http.Do(req)
		dur := time.Since(start)
		if err != nil {
			c.observeOne(path, 0, dur)
			lastErr = err
			if !shouldRetryNetErr(err) {
				return err
			}
		} else {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			c.observeOne(path, resp.StatusCode, dur)
			switch {
			case resp.StatusCode == http.StatusOK:
				if readErr != nil {
					return fmt.Errorf("httpx: read body: %w", readErr)
				}
				if err := json.Unmarshal(body, out); err != nil {
					return fmt.Errorf("httpx: decode %s: %w", u.String(), err)
				}
				return nil
			case resp.StatusCode == http.StatusNotFound:
				return ErrNotFound
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = fmt.Errorf("httpx: upstream status %d: %s", resp.StatusCode, truncate(body, 256))
				if c.log != nil {
					c.log.Warn().Int("status", resp.StatusCode).Str("url", u.String()).Int("attempt", attempt).Msg("retrying upstream")
				}
			default:
				return fmt.Errorf("httpx: %s -> %d: %s", u.String(), resp.StatusCode, truncate(body, 256))
			}
		}

		if attempt == c.maxRetry {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("httpx: exhausted retries")
	}
	return lastErr
}

func (c *Client) observeOne(endpoint string, status int, dur time.Duration) {
	if c.observe != nil {
		c.observe(endpoint, status, dur)
	}
}

func joinPath(base, rel string) string {
	if base == "" {
		return rel
	}
	if base[len(base)-1] == '/' && len(rel) > 0 && rel[0] == '/' {
		return base + rel[1:]
	}
	if base[len(base)-1] != '/' && (len(rel) == 0 || rel[0] != '/') {
		return base + "/" + rel
	}
	return base + rel
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * time.Second
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base/2 + jitter
}

func shouldRetryNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
