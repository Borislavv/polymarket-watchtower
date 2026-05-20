package eventpage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// EventPageClient is the seam used by the usecase layer.
type EventPageClient interface {
	FetchEventPage(ctx context.Context, eventSlug string) (*EventPagePayload, error)
}

// Client is the production implementation. It depends on a
// BuildIDResolver and an HTTP client; both are overridable for
// tests.
type Client struct {
	jsonBaseURL string // template, %s = buildId, %s = slug, %s = slug
	htmlBaseURL string

	http     *http.Client
	resolver *BuildIDResolver
	log      *zerolog.Logger
	now      func() time.Time

	// readCap bounds the JSON body we'll read. Defaults to 8 MB.
	// Anything larger is almost certainly a CDN error page, not
	// Polymarket data.
	readCap int64
}

// ClientConfig wires the client.
type ClientConfig struct {
	// HTMLBaseURL backs both the resolver (HTML scrape) and the
	// JSON endpoint (same origin). Defaults to "https://polymarket.com".
	HTMLBaseURL string
	// Resolver is required. Tests can pass a fake.
	Resolver *BuildIDResolver
	// HTTPClient is overridable for tests.
	HTTPClient *http.Client
	// Logger is optional.
	Logger *zerolog.Logger
	// Clock is overridable for tests.
	Clock func() time.Time
	// ReadCap bounds the JSON body read size.
	ReadCap int64
}

// NewClient wires a Client. The resolver MUST be supplied so the
// caller controls its TTL + HTTP knobs.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Resolver == nil {
		return nil, errors.New("eventpage: resolver required")
	}
	if cfg.HTMLBaseURL == "" {
		cfg.HTMLBaseURL = "https://polymarket.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 8 * time.Second}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.ReadCap <= 0 {
		cfg.ReadCap = 8 << 20
	}
	return &Client{
		jsonBaseURL: cfg.HTMLBaseURL + "/_next/data/%s/en/event/%s.json?slug=%s",
		htmlBaseURL: cfg.HTMLBaseURL,
		http:        cfg.HTTPClient,
		resolver:    cfg.Resolver,
		log:         cfg.Logger,
		now:         cfg.Clock,
		readCap:     cfg.ReadCap,
	}, nil
}

// FetchEventPage resolves the buildId, fetches the event page JSON,
// and returns the parsed payload. On a stale-buildId 404 it
// refreshes the resolver once and retries.
//
// Errors are returned to the caller for telemetry; the usecase
// layer translates them into a silent "unavailable" prompt slot
// so the alert flow is never blocked.
func (c *Client) FetchEventPage(ctx context.Context, eventSlug string) (*EventPagePayload, error) {
	if eventSlug == "" {
		return nil, errors.New("eventpage: event_slug required")
	}
	id, err := c.resolver.Resolve(ctx, eventSlug, false)
	if err != nil {
		return nil, fmt.Errorf("eventpage: resolve build_id: %w", err)
	}
	payload, status, err := c.fetchJSON(ctx, id, eventSlug)
	if err == nil {
		payload.BuildID = id
		return payload, nil
	}
	// One forced refresh on 404 — covers the common case where a
	// Vercel deploy rotated the buildId between resolver fetches.
	if status == http.StatusNotFound {
		fresh, refreshErr := c.resolver.Resolve(ctx, eventSlug, true)
		if refreshErr != nil {
			return nil, fmt.Errorf("eventpage: refresh build_id after 404: %w", refreshErr)
		}
		if fresh == id {
			return nil, fmt.Errorf("eventpage: 404 with unchanged build_id %q", id)
		}
		payload, _, err = c.fetchJSON(ctx, fresh, eventSlug)
		if err != nil {
			return nil, err
		}
		payload.BuildID = fresh
		return payload, nil
	}
	return nil, err
}

func (c *Client) fetchJSON(ctx context.Context, buildID, eventSlug string) (*EventPagePayload, int, error) {
	url := fmt.Sprintf(c.jsonBaseURL, buildID, eventSlug, eventSlug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("eventpage: build request: %w", err)
	}
	req.Header.Set("User-Agent", "polymarket-watchtower/1.0 (+contextual-fetcher)")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("eventpage: fetch json: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, fmt.Errorf("eventpage: json 404 (build_id %q likely stale)", buildID)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, resp.StatusCode, fmt.Errorf("eventpage: json status %d: %s", resp.StatusCode, string(body))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.readCap))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("eventpage: read body: %w", err)
	}
	payload, err := parsePayload(eventSlug, raw, c.now())
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}
