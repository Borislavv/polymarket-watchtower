package eventpage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// BuildIDResolver fetches polymarket.com HTML, extracts the
// `__NEXT_DATA__.buildId` (Next.js cache-bust string), and caches it
// in memory for ResolverTTL. The buildId rotates on every Vercel
// deploy; callers refresh on 404 by setting force=true.
//
// Concurrency: a singleflight pattern guards refresh so one stampede
// (many alerts firing during a Polymarket deploy) issues exactly one
// HTML fetch.
type BuildIDResolver struct {
	htmlURL     string // template; %s substituted with event slug
	fallbackURL string // hit when the event slug page errors

	http *http.Client
	log  *zerolog.Logger
	now  func() time.Time

	ttl time.Duration

	mu       sync.Mutex
	cachedID string
	cachedAt time.Time
	inFlight *buildIDFlight
}

// BuildIDResolverConfig wires the resolver.
type BuildIDResolverConfig struct {
	// HTMLBaseURL is the public Polymarket site. Defaults to
	// "https://polymarket.com" when empty.
	HTMLBaseURL string
	// Timeout is the per-HTML-fetch deadline. Defaults to 5s.
	Timeout time.Duration
	// TTL bounds how long a cached buildId survives between forced
	// refreshes. Defaults to 30m.
	TTL time.Duration
	// HTTPClient is overridable for tests.
	HTTPClient *http.Client
	Logger     *zerolog.Logger
	Clock      func() time.Time
}

// NewBuildIDResolver wires defaults around the supplied config.
func NewBuildIDResolver(cfg BuildIDResolverConfig) *BuildIDResolver {
	if cfg.HTMLBaseURL == "" {
		cfg.HTMLBaseURL = "https://polymarket.com"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &BuildIDResolver{
		htmlURL:     cfg.HTMLBaseURL + "/event/%s",
		fallbackURL: cfg.HTMLBaseURL + "/",
		http:        cfg.HTTPClient,
		log:         cfg.Logger,
		now:         cfg.Clock,
		ttl:         cfg.TTL,
	}
}

// Resolve returns the cached buildId or fetches a fresh one when
// force is true or the cache has aged past TTL. The eventSlug is
// used to target the most relevant SSR page; an empty slug falls
// back to the site root.
func (r *BuildIDResolver) Resolve(ctx context.Context, eventSlug string, force bool) (string, error) {
	r.mu.Lock()
	if !force && r.cachedID != "" && r.now().Sub(r.cachedAt) < r.ttl {
		id := r.cachedID
		r.mu.Unlock()
		return id, nil
	}
	// Coalesce concurrent refreshes — first caller does the work,
	// the rest wait on the in-flight channel.
	if r.inFlight != nil {
		ch := r.inFlight.done
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		r.mu.Lock()
		id := r.cachedID
		r.mu.Unlock()
		if id == "" {
			return "", errors.New("eventpage: build_id resolve coalesced but cache empty")
		}
		return id, nil
	}
	flight := &buildIDFlight{done: make(chan struct{})}
	r.inFlight = flight
	prev := r.cachedID
	r.mu.Unlock()

	id, err := r.fetch(ctx, eventSlug)

	r.mu.Lock()
	if err == nil {
		if id != prev && r.log != nil {
			r.log.Info().Str("old", prev).Str("new", id).Msg("eventpage: build_id changed")
		}
		r.cachedID = id
		r.cachedAt = r.now()
	}
	close(flight.done)
	r.inFlight = nil
	r.mu.Unlock()
	return id, err
}

type buildIDFlight struct {
	done chan struct{}
}

// fetch retrieves /event/<slug> (or the root) and extracts the
// buildId. We try __NEXT_DATA__ JSON first (cheap + canonical),
// then fall back to a regex over `/_next/static/<id>/` references
// the page emits.
func (r *BuildIDResolver) fetch(ctx context.Context, eventSlug string) (string, error) {
	url := r.fallbackURL
	if eventSlug != "" {
		url = fmt.Sprintf(r.htmlURL, eventSlug)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("eventpage: build_id request: %w", err)
	}
	// Polymarket's CDN keys off a sensible User-Agent. We don't
	// pretend to be a browser, but a non-empty UA avoids 403s seen
	// from naked Go clients.
	req.Header.Set("User-Agent", "polymarket-watchtower/1.0 (+contextual-fetcher)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("eventpage: build_id fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("eventpage: build_id status %d", resp.StatusCode)
	}
	// Cap the read so a slow/giant page can't blow memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB
	if err != nil {
		return "", fmt.Errorf("eventpage: build_id read: %w", err)
	}
	id := ExtractBuildIDFromHTML(body)
	if id == "" {
		return "", errors.New("eventpage: build_id not found in html")
	}
	return id, nil
}

// nextDataRE locates the JSON blob the Next.js runtime hydrates from.
var nextDataRE = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(\{.*?\})</script>`)

// staticAssetRE picks up `/_next/static/<buildId>/` paths the page
// emits as fallback (CSS/JS hrefs). We prefer values starting with
// "build-" because Polymarket uses that prefix today; if no
// build-* candidate is present we fall back to the first match.
var staticAssetRE = regexp.MustCompile(`/_next/static/([A-Za-z0-9_\-]+)/`)

// ExtractBuildIDFromHTML is exported so tests can hit it directly.
// Order:
//  1. __NEXT_DATA__ JSON's top-level buildId;
//  2. regex over `/_next/static/<id>/` paths, preferring build-*.
//
// Returns "" when nothing usable is found.
func ExtractBuildIDFromHTML(body []byte) string {
	if m := nextDataRE.FindSubmatch(body); len(m) == 2 {
		var nd struct {
			BuildID string `json:"buildId"`
		}
		if err := json.Unmarshal(m[1], &nd); err == nil && nd.BuildID != "" {
			return nd.BuildID
		}
	}
	matches := staticAssetRE.FindAllSubmatch(body, -1)
	for _, m := range matches {
		if len(m) == 2 && strings.HasPrefix(string(m[1]), "build-") {
			return string(m[1])
		}
	}
	if len(matches) > 0 && len(matches[0]) == 2 {
		return string(matches[0][1])
	}
	return ""
}

// Cached returns the buildId currently in cache plus the timestamp
// it was last set, or ("", zero) when uncached. Used by tests +
// observability.
func (r *BuildIDResolver) Cached() (string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cachedID, r.cachedAt
}
