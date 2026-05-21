package eventpage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// EventPageClient is the seam used by the usecase layer.
type EventPageClient interface {
	FetchEventPage(ctx context.Context, eventSlug string) (*EventPagePayload, error)
}

// AliasStore is the optional persistence seam for canonical-slug
// aliases. Production wires `*repository.EventPageRepository`;
// tests pass an in-memory implementation. nil disables persistence
// (the in-memory alias cache still works).
type AliasStore interface {
	UpsertEventSlugAlias(ctx context.Context, original, canonical, source string) error
	GetEventSlugAlias(ctx context.Context, original string) (canonical string, ok bool, err error)
}

// Client is the production implementation. It owns the JSON fetch
// pipeline AND the v10.5 redirect / canonical-slug / HTML-fallback
// fix.
type Client struct {
	htmlBaseURL string // template, %s = slug

	http     *http.Client
	resolver *BuildIDResolver
	aliases  AliasStore
	met      MetricsSink
	log      *zerolog.Logger
	now      func() time.Time

	maxRedirects int
	readCap      int64

	// In-memory alias cache. The DB-backed AliasStore (when wired)
	// promotes aliases across restarts. This map is the hot-path
	// cache so we don't round-trip Postgres on every fetch.
	aliasMu       sync.RWMutex
	canonicalSlug map[string]string
}

// MetricsSink is the small slice of *metrics.Metrics the client
// uses. Defined as an interface so tests pass a nil-noop and the
// infra/metrics package stays out of the eventpage import graph.
type MetricsSink interface {
	ObserveRedirect(status string)
	ObserveRedirectFailure(reason string)
	ObserveBuildIDRefresh(reason string)
	ObserveSlugAlias()
	ObserveContextStale(reason string)
}

// noopMetrics is the implicit nil-sink.
type noopMetrics struct{}

func (noopMetrics) ObserveRedirect(string)        {}
func (noopMetrics) ObserveRedirectFailure(string) {}
func (noopMetrics) ObserveBuildIDRefresh(string)  {}
func (noopMetrics) ObserveSlugAlias()             {}
func (noopMetrics) ObserveContextStale(string)    {}

// ClientConfig wires the client.
type ClientConfig struct {
	HTMLBaseURL  string
	Resolver     *BuildIDResolver
	AliasStore   AliasStore
	HTTPClient   *http.Client
	Logger       *zerolog.Logger
	Clock        func() time.Time
	ReadCap      int64
	MaxRedirects int
	Metrics      MetricsSink
}

// NewClient wires a Client.
//
// IMPORTANT: NewClient REPLACES the http.Client's redirect policy
// with one that refuses all automatic redirects. We need to inspect
// the 307 from the Next.js data route ourselves — Go's default
// follow-on-307 would silently land on the HTML page and our JSON
// parser would fail.
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
	// Disable automatic redirect following so the 307 path is ours
	// to inspect + classify. We still respect ctx + Transport.
	cfg.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.ReadCap <= 0 {
		cfg.ReadCap = 8 << 20
	}
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = 5
	}
	if cfg.Metrics == nil {
		cfg.Metrics = noopMetrics{}
	}
	return &Client{
		htmlBaseURL:   cfg.HTMLBaseURL,
		http:          cfg.HTTPClient,
		resolver:      cfg.Resolver,
		aliases:       cfg.AliasStore,
		met:           cfg.Metrics,
		log:           cfg.Logger,
		now:           cfg.Clock,
		readCap:       cfg.ReadCap,
		maxRedirects:  cfg.MaxRedirects,
		canonicalSlug: make(map[string]string),
	}, nil
}

// FetchEventPage is the v10.5 robust fetch path:
//
//  1. Resolve the buildId (cached).
//  2. Look up a known canonical slug alias; use it when present.
//  3. Issue the JSON data-route request. If we get 2xx, parse.
//  4. If we get a 301/302/303/307/308 with x-nextjs-redirect /
//     Location pointing at the HTML page, fall through to the HTML
//     fallback that extracts __NEXT_DATA__ inline.
//  5. If we get a 404 (stale buildId), refresh buildId once + retry.
//  6. Every failure returns a typed *FetchError with category +
//     metadata; never the legacy `eventpage: json status 307` string.
//
// The HTML fallback is the canonical hot path today — Polymarket's
// data route returns 307 → HTML for every event slug at the time of
// writing. Treating that as a fatal error is the bug we're fixing.
func (c *Client) FetchEventPage(ctx context.Context, eventSlug string) (*EventPagePayload, error) {
	if eventSlug == "" {
		return nil, &FetchError{Category: CategoryUnknown, Underlying: errors.New("event_slug required")}
	}

	originalSlug := eventSlug
	slug := c.resolveAlias(ctx, eventSlug)

	id, err := c.resolver.Resolve(ctx, slug, false)
	if err != nil {
		return nil, &FetchError{
			Category:     CategoryNetworkTimeout,
			OriginalSlug: originalSlug,
			Underlying:   err,
		}
	}

	payload, attempt := c.tryFetch(ctx, originalSlug, slug, id, 0)
	if payload != nil {
		return payload, nil
	}
	// `attempt` carries the first-attempt FetchError. Apply the
	// retry policy for the recoverable categories.
	switch attempt.Category {
	case CategoryStaleBuildID:
		c.met.ObserveBuildIDRefresh("stale_build_id")
		fresh, refreshErr := c.resolver.Resolve(ctx, slug, true)
		if refreshErr != nil || fresh == id {
			return nil, attempt
		}
		if p2, e2 := c.tryFetch(ctx, originalSlug, slug, fresh, 1); p2 != nil {
			return p2, nil
		} else {
			return nil, e2
		}
	case CategoryRedirectToCanonicalSlug:
		// Stash the alias for this AND future fetches.
		if attempt.CanonicalSlug != "" && attempt.CanonicalSlug != slug {
			c.stashAlias(ctx, originalSlug, attempt.CanonicalSlug, "redirect")
			if p2, e2 := c.tryFetch(ctx, originalSlug, attempt.CanonicalSlug, id, 1); p2 != nil {
				return p2, nil
			} else {
				return nil, e2
			}
		}
		return nil, attempt
	}
	return nil, attempt
}

// tryFetch executes ONE round-trip: JSON → optional HTML fallback.
// Returns either the parsed payload OR a typed FetchError. Never
// retries; the outer FetchEventPage owns the retry policy.
func (c *Client) tryFetch(ctx context.Context, originalSlug, slug, buildID string, retry int) (*EventPagePayload, *FetchError) {
	jsonURL := fmt.Sprintf("%s/_next/data/%s/en/event/%s.json?slug=%s",
		c.htmlBaseURL, buildID, slug, slug)
	resp, err := c.do(ctx, jsonURL)
	if err != nil {
		return nil, &FetchError{
			Category:     CategoryNetworkTimeout,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			FinalURL:     jsonURL,
			RetryCount:   retry,
			Underlying:   err,
		}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return c.parseJSONBody(originalSlug, slug, buildID, resp, retry)

	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return c.handleRedirect(ctx, originalSlug, slug, buildID, resp, retry)

	case http.StatusNotFound:
		return nil, &FetchError{
			Category:     CategoryStaleBuildID,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			Status:       resp.StatusCode,
			FinalURL:     jsonURL,
			RetryCount:   retry,
		}
	}
	// Any other non-2xx: surface as a structured non_200 error.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	c.met.ObserveRedirectFailure("non_200")
	return nil, &FetchError{
		Category:     CategoryJSONStatusNon200,
		OriginalSlug: originalSlug,
		BuildID:      buildID,
		Status:       resp.StatusCode,
		FinalURL:     jsonURL,
		ContentType:  resp.Header.Get("Content-Type"),
		BodyPreview:  capPreview(string(body), 300),
		RetryCount:   retry,
	}
}

// handleRedirect resolves the redirect target. Three cases:
//
//   - x-nextjs-redirect points at /event/<same-slug> → HTML fallback.
//   - Location/x-nextjs-redirect points at /event/<canonical-slug>
//     → return CategoryRedirectToCanonicalSlug so the outer caller
//     can stash the alias and retry the JSON path with the canonical
//     slug.
//   - Location points at another _next/data/.json URL → follow.
//
// Loop detection: counts hops through `redirectChain` and bails on
// MaxRedirects.
func (c *Client) handleRedirect(ctx context.Context, originalSlug, slug, buildID string, resp *http.Response, retry int) (*EventPagePayload, *FetchError) {
	target := strings.TrimSpace(resp.Header.Get("X-Nextjs-Redirect"))
	if target == "" {
		target = resp.Header.Get("Location")
	}
	c.met.ObserveRedirect(fmt.Sprintf("%d", resp.StatusCode))
	if target == "" {
		c.met.ObserveRedirectFailure("missing_location")
		return nil, &FetchError{
			Category:     CategoryJSONStatusNon200,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			Status:       resp.StatusCode,
			RetryCount:   retry,
			Underlying:   errors.New("redirect with empty Location"),
		}
	}

	// Resolve relative redirect to absolute URL.
	abs := c.resolveTarget(target, resp.Request.URL)

	// Inspect target. Two classes matter: an /event/<slug> HTML
	// page (the v10.5 canonical hot path) vs another _next/data
	// JSON URL.
	pathPart, querySlug := parseEventTarget(abs)
	switch {
	case strings.Contains(abs.Path, "/_next/data/"):
		// Follow it as JSON. Loop-detect via retry count + cap.
		if retry >= c.maxRedirects {
			c.met.ObserveRedirectFailure("loop_or_cap")
			return nil, &FetchError{
				Category:     CategoryRedirectLoop,
				OriginalSlug: originalSlug,
				BuildID:      buildID,
				Status:       resp.StatusCode,
				Location:     target,
				FinalURL:     abs.String(),
				RetryCount:   retry,
			}
		}
		return c.fetchJSONURL(ctx, originalSlug, slug, buildID, abs.String(), retry+1)

	case pathPart != "":
		// The redirect is to /event/<X>. Two sub-cases:
		//   a) X == slug — same event, JSON unavailable, switch to HTML fallback.
		//   b) X != slug — canonical alias.
		canonical := pathPart
		if querySlug != "" && querySlug != canonical {
			canonical = querySlug
		}
		if canonical != "" && canonical != slug {
			// Surface as RedirectToCanonicalSlug; outer caller
			// stashes alias + retries.
			return nil, &FetchError{
				Category:      CategoryRedirectToCanonicalSlug,
				OriginalSlug:  originalSlug,
				CanonicalSlug: canonical,
				BuildID:       buildID,
				Status:        resp.StatusCode,
				Location:      target,
				FinalURL:      abs.String(),
				RetryCount:    retry,
			}
		}
		// Same-slug HTML fallback — the canonical hot path today.
		return c.fetchHTMLFallback(ctx, originalSlug, slug, buildID, abs.String(), retry+1)
	}

	// Unsupported redirect target: not /event/<slug> and not /_next/data.
	c.met.ObserveRedirectFailure("unsupported_target")
	return nil, &FetchError{
		Category:     CategoryJSONContentTypeUnexpected,
		OriginalSlug: originalSlug,
		BuildID:      buildID,
		Status:       resp.StatusCode,
		Location:     target,
		FinalURL:     abs.String(),
		ContentType:  resp.Header.Get("Content-Type"),
		RetryCount:   retry,
	}
}

// fetchJSONURL re-fetches an explicit JSON URL (used when a redirect
// target points at another _next/data JSON path).
func (c *Client) fetchJSONURL(ctx context.Context, originalSlug, slug, buildID, jsonURL string, retry int) (*EventPagePayload, *FetchError) {
	resp, err := c.do(ctx, jsonURL)
	if err != nil {
		return nil, &FetchError{Category: CategoryNetworkTimeout, OriginalSlug: originalSlug, BuildID: buildID, FinalURL: jsonURL, RetryCount: retry, Underlying: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return c.parseJSONBody(originalSlug, slug, buildID, resp, retry)
	}
	// One more redirect? Recurse via handleRedirect — but cap by retry.
	if resp.StatusCode/100 == 3 {
		return c.handleRedirect(ctx, originalSlug, slug, buildID, resp, retry)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return nil, &FetchError{
		Category:     CategoryJSONStatusNon200,
		OriginalSlug: originalSlug,
		BuildID:      buildID,
		Status:       resp.StatusCode,
		FinalURL:     jsonURL,
		ContentType:  resp.Header.Get("Content-Type"),
		BodyPreview:  capPreview(string(body), 300),
		RetryCount:   retry,
	}
}

// fetchHTMLFallback fetches /event/<slug> as HTML and parses
// __NEXT_DATA__ inline. The inner shape carries the SAME `pageProps.
// dehydratedState.queries[]` payload the JSON data route would have
// served, just wrapped in an extra `props` envelope.
func (c *Client) fetchHTMLFallback(ctx context.Context, originalSlug, slug, buildID, htmlURL string, retry int) (*EventPagePayload, *FetchError) {
	if retry > c.maxRedirects {
		c.met.ObserveRedirectFailure("loop_or_cap")
		return nil, &FetchError{Category: CategoryRedirectLoop, OriginalSlug: originalSlug, BuildID: buildID, FinalURL: htmlURL, RetryCount: retry}
	}
	resp, err := c.doHTML(ctx, htmlURL)
	if err != nil {
		return nil, &FetchError{Category: CategoryNetworkTimeout, OriginalSlug: originalSlug, BuildID: buildID, FinalURL: htmlURL, RetryCount: retry, Underlying: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 3 {
		return c.handleRedirect(ctx, originalSlug, slug, buildID, resp, retry)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &FetchError{
			Category:     CategoryCanonicalSlugFailed,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			Status:       resp.StatusCode,
			FinalURL:     htmlURL,
			ContentType:  resp.Header.Get("Content-Type"),
			BodyPreview:  capPreview(string(body), 300),
			RetryCount:   retry,
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.readCap))
	if err != nil {
		return nil, &FetchError{Category: CategoryCanonicalSlugFailed, OriginalSlug: originalSlug, BuildID: buildID, FinalURL: htmlURL, RetryCount: retry, Underlying: err}
	}
	// Extract __NEXT_DATA__ JSON. The inner `.props.pageProps...`
	// shape matches what parsePayload accepts post-unwrap.
	innerJSON, gotBuildID := extractNextDataJSON(body)
	if len(innerJSON) == 0 {
		c.met.ObserveRedirectFailure("html_no_next_data")
		return nil, &FetchError{
			Category:     CategoryCanonicalSlugFailed,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			Status:       resp.StatusCode,
			FinalURL:     htmlURL,
			ContentType:  resp.Header.Get("Content-Type"),
			BodyPreview:  capPreview(string(body), 300),
			RetryCount:   retry,
			Underlying:   errors.New("__NEXT_DATA__ not found in HTML"),
		}
	}
	payload, err := parsePayload(slug, innerJSON, c.now())
	if err != nil {
		return nil, &FetchError{Category: CategoryJSONParseFailed, OriginalSlug: originalSlug, BuildID: buildID, FinalURL: htmlURL, RetryCount: retry, Underlying: err}
	}
	if gotBuildID != "" {
		payload.BuildID = gotBuildID
	} else {
		payload.BuildID = buildID
	}
	if c.log != nil {
		c.log.Debug().
			Str("original_slug", originalSlug).
			Str("slug", slug).
			Str("build_id", payload.BuildID).
			Str("final_url", htmlURL).
			Int("retry", retry).
			Msg("eventpage: HTML __NEXT_DATA__ fallback succeeded")
	}
	return payload, nil
}

// parseJSONBody parses a 200-status JSON response body. Carries
// content-type validation + body preview on failure.
func (c *Client) parseJSONBody(originalSlug, slug, buildID string, resp *http.Response, retry int) (*EventPagePayload, *FetchError) {
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.readCap))
	if err != nil {
		return nil, &FetchError{Category: CategoryNetworkTimeout, OriginalSlug: originalSlug, BuildID: buildID, RetryCount: retry, Underlying: err}
	}
	// Be tolerant: Polymarket sometimes serves application/json,
	// sometimes a missing/empty content-type. Reject only when the
	// body's first non-space byte clearly isn't JSON.
	first := firstNonSpace(body)
	if first != '{' && first != '[' {
		return nil, &FetchError{
			Category:     CategoryJSONContentTypeUnexpected,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			Status:       resp.StatusCode,
			ContentType:  ct,
			BodyPreview:  capPreview(string(body), 300),
			RetryCount:   retry,
		}
	}
	payload, err := parsePayload(slug, body, c.now())
	if err != nil {
		return nil, &FetchError{
			Category:     CategoryJSONParseFailed,
			OriginalSlug: originalSlug,
			BuildID:      buildID,
			Status:       resp.StatusCode,
			ContentType:  ct,
			BodyPreview:  capPreview(string(body), 300),
			RetryCount:   retry,
			Underlying:   err,
		}
	}
	payload.BuildID = buildID
	return payload, nil
}

// do issues a GET with the JSON-request headers.
func (c *Client) do(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "polymarket-watchtower/1.0 (+contextual-fetcher)")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en")
	return c.http.Do(req)
}

// doHTML issues a GET with HTML-request headers (used for the
// __NEXT_DATA__ fallback path).
func (c *Client) doHTML(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "polymarket-watchtower/1.0 (+contextual-fetcher)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en")
	return c.http.Do(req)
}

// resolveTarget normalises a redirect target. Relative paths are
// resolved against the request URL; absolute URLs are returned as-is.
func (c *Client) resolveTarget(target string, base *url.URL) *url.URL {
	tu, err := url.Parse(target)
	if err != nil {
		return &url.URL{Path: target}
	}
	if tu.IsAbs() {
		return tu
	}
	if base == nil {
		// Fall back to the configured base.
		bu, _ := url.Parse(c.htmlBaseURL)
		return bu.ResolveReference(tu)
	}
	return base.ResolveReference(tu)
}

// parseEventTarget extracts the slug portion of an /event/<slug>
// path AND its `?slug=` query if present.
func parseEventTarget(u *url.URL) (pathSlug, querySlug string) {
	if u == nil {
		return "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "event" {
			pathSlug = parts[i+1]
			break
		}
	}
	querySlug = u.Query().Get("slug")
	return pathSlug, querySlug
}

// resolveAlias looks up a canonical-slug alias for `slug`. Order:
//
//  1. In-memory cache (hot path).
//  2. Persistent AliasStore (if wired).
//
// Returns the original slug on miss.
func (c *Client) resolveAlias(ctx context.Context, slug string) string {
	c.aliasMu.RLock()
	cached, ok := c.canonicalSlug[slug]
	c.aliasMu.RUnlock()
	if ok && cached != "" {
		return cached
	}
	if c.aliases == nil {
		return slug
	}
	canonical, ok, err := c.aliases.GetEventSlugAlias(ctx, slug)
	if err != nil || !ok || canonical == "" {
		return slug
	}
	c.aliasMu.Lock()
	c.canonicalSlug[slug] = canonical
	c.aliasMu.Unlock()
	return canonical
}

// stashAlias records (original → canonical) in the in-memory cache
// AND the persistent AliasStore (when wired).
func (c *Client) stashAlias(ctx context.Context, original, canonical, source string) {
	if original == "" || canonical == "" || original == canonical {
		return
	}
	c.aliasMu.Lock()
	c.canonicalSlug[original] = canonical
	c.aliasMu.Unlock()
	c.met.ObserveSlugAlias()
	if c.aliases != nil {
		if err := c.aliases.UpsertEventSlugAlias(ctx, original, canonical, source); err != nil && c.log != nil {
			c.log.Warn().Err(err).Str("original", original).Str("canonical", canonical).Msg("eventpage: persist slug alias failed")
		}
	}
	if c.log != nil {
		c.log.Info().Str("original_slug", original).Str("canonical_slug", canonical).Str("source", source).Msg("eventpage: canonical slug recorded")
	}
}

// extractNextDataJSON pulls the inner `<script id="__NEXT_DATA__">`
// JSON from a Polymarket HTML page and returns the part our parser
// understands (the wrapping is `.props` on top of the data-route's
// envelope). Returns the inner pageProps-bearing JSON + the buildId
// the page declared.
func extractNextDataJSON(body []byte) ([]byte, string) {
	m := nextDataRE.FindSubmatch(body)
	if len(m) != 2 {
		return nil, ""
	}
	var nd struct {
		BuildID string         `json:"buildId"`
		Props   map[string]any `json:"props"`
	}
	if err := unmarshalJSON(m[1], &nd); err != nil {
		return nil, ""
	}
	// The data route emits `{pageProps:...}`; the HTML page emits
	// `{props:{pageProps:...}, buildId, page, query, ...}`. Re-emit
	// the inner `props` so parsePayload sees the same shape it knew.
	if len(nd.Props) == 0 {
		// Fall back to the raw body — parsePayload tolerates extra
		// top-level fields it doesn't recognise.
		return m[1], nd.BuildID
	}
	out, err := marshalJSON(nd.Props)
	if err != nil {
		return m[1], nd.BuildID
	}
	return out, nd.BuildID
}

func firstNonSpace(b []byte) byte {
	for _, ch := range b {
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			continue
		}
		return ch
	}
	return 0
}

func capPreview(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
