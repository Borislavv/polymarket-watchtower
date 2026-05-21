package eventpage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- buildId extraction --------------------------------------------------

func TestExtractBuildID_FromNextData(t *testing.T) {
	html := []byte(`<html><body>` +
		`<script id="__NEXT_DATA__" type="application/json">` +
		`{"buildId":"build-abc123","props":{}}` +
		`</script></body></html>`)
	got := ExtractBuildIDFromHTML(html)
	if got != "build-abc123" {
		t.Fatalf("buildId: got %q want build-abc123", got)
	}
}

func TestExtractBuildID_FromStaticAssetFallback(t *testing.T) {
	// No __NEXT_DATA__; only static asset references. We prefer
	// "build-*" entries.
	html := []byte(`<html><link href="/_next/static/chunks-XYZ/main.css">` +
		`<script src="/_next/static/build-FALLBACK-42/abc.js"></script></html>`)
	got := ExtractBuildIDFromHTML(html)
	if got != "build-FALLBACK-42" {
		t.Fatalf("buildId: got %q want build-FALLBACK-42", got)
	}
}

func TestExtractBuildID_NoMatchReturnsEmpty(t *testing.T) {
	if got := ExtractBuildIDFromHTML([]byte(`<html><body>nothing here</body></html>`)); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// --- resolver: cache + force refresh + singleflight ---------------------

func TestBuildIDResolver_CachesUntilForced(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-X"}</script>`))
	}))
	defer srv.Close()
	r := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	id, err := r.Resolve(context.Background(), "any", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "build-X" {
		t.Fatalf("id: got %q", id)
	}
	// Second call within TTL: no extra HTTP.
	if _, err := r.Resolve(context.Background(), "any", false); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 HTTP hit, got %d", hits)
	}
	// Forced refresh hits again.
	if _, err := r.Resolve(context.Background(), "any", true); err != nil {
		t.Fatalf("resolve 3: %v", err)
	}
	if hits != 2 {
		t.Fatalf("expected 2 HTTP hits after force, got %d", hits)
	}
}

// --- client: URL construction + parse end-to-end ------------------------

func TestClient_FetchEventPage_HitsBuildIDURLAndParses(t *testing.T) {
	fixture := mustReadFixture(t, "event_page.json")
	var seenPath string
	var seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_next/data/"):
			seenPath = r.URL.Path
			seenQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		case strings.HasPrefix(r.URL.Path, "/event/"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-TfctsWXpff2fKS"}</script>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pl, err := c.FetchEventPage(context.Background(), "texas-republican-senate-primary-winner")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	wantPath := "/_next/data/build-TfctsWXpff2fKS/en/event/texas-republican-senate-primary-winner.json"
	if seenPath != wantPath {
		t.Errorf("path: got %q want %q", seenPath, wantPath)
	}
	q, _ := url.ParseQuery(seenQuery)
	if got := q.Get("slug"); got != "texas-republican-senate-primary-winner" {
		t.Errorf("?slug query: got %q want texas-republican-senate-primary-winner", got)
	}
	// Event parsed.
	if pl.Event.Title == "" {
		t.Errorf("event title empty")
	}
	if pl.Event.ContextDescription == "" {
		t.Errorf("context_description empty")
	}
	// Markets parsed including outcomes + clob token ids.
	if len(pl.Markets) != 2 {
		t.Fatalf("markets: got %d want 2", len(pl.Markets))
	}
	var paxton *EventPageMarket
	for i := range pl.Markets {
		if pl.Markets[i].ConditionID == "0xpaxton" {
			paxton = &pl.Markets[i]
		}
	}
	if paxton == nil {
		t.Fatal("paxton market missing")
	}
	if len(paxton.Outcomes) != 2 || paxton.Outcomes[0] != "Yes" {
		t.Errorf("outcomes parse failed: %+v", paxton.Outcomes)
	}
	if len(paxton.CLOBTokenIDs) != 2 {
		t.Errorf("clob token ids parse failed: %+v", paxton.CLOBTokenIDs)
	}
	// Annotations parsed with priceBefore/priceAfter/sources.
	if len(pl.Annotations) != 3 {
		t.Fatalf("annotations: got %d want 3", len(pl.Annotations))
	}
	var paxtonPoll *EventAnnotation
	for i := range pl.Annotations {
		if strings.Contains(pl.Annotations[i].Title, "Final pre-runoff poll") {
			paxtonPoll = &pl.Annotations[i]
		}
	}
	if paxtonPoll == nil {
		t.Fatal("paxton poll annotation missing")
	}
	if paxtonPoll.PriceBefore == nil || *paxtonPoll.PriceBefore != 0.54 {
		t.Errorf("priceBefore wrong: %+v", paxtonPoll.PriceBefore)
	}
	if paxtonPoll.PriceAfter == nil || *paxtonPoll.PriceAfter != 0.61 {
		t.Errorf("priceAfter wrong: %+v", paxtonPoll.PriceAfter)
	}
	if len(paxtonPoll.Sources) != 1 || paxtonPoll.Sources[0].URL == "" {
		t.Errorf("sources parse failed: %+v", paxtonPoll.Sources)
	}
	// Tags + similar markets captured.
	if len(pl.SimilarMarkets) != 1 {
		t.Errorf("similar markets: got %d want 1", len(pl.SimilarMarkets))
	}
	if len(pl.Tags) != 2 {
		t.Errorf("tags: got %d want 2", len(pl.Tags))
	}
	// RawQueryKeys lists everything we saw — used by telemetry.
	if len(pl.RawQueryKeys) != 4 {
		t.Errorf("raw query keys: got %d want 4: %+v", len(pl.RawQueryKeys), pl.RawQueryKeys)
	}
}

// --- client: stale buildId refresh ---------------------------------------

func TestClient_RetriesOnStaleBuildID(t *testing.T) {
	fixture := mustReadFixture(t, "event_page.json")
	var jsonAttempts int
	var htmlAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_next/data/build-OLD/"):
			jsonAttempts++
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/_next/data/build-NEW/"):
			jsonAttempts++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		case strings.HasPrefix(r.URL.Path, "/event/"):
			htmlAttempts++
			// First HTML returns OLD, second returns NEW.
			if htmlAttempts == 1 {
				_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-OLD"}</script>`))
			} else {
				_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-NEW"}</script>`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pl, err := c.FetchEventPage(context.Background(), "x")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pl.BuildID != "build-NEW" {
		t.Errorf("buildID: got %q want build-NEW", pl.BuildID)
	}
	if jsonAttempts != 2 {
		t.Errorf("json attempts: got %d want 2", jsonAttempts)
	}
	if htmlAttempts != 2 {
		t.Errorf("html attempts: got %d want 2", htmlAttempts)
	}
}

// --- v10.5 redirect + canonical-slug + HTML fallback --------------------

// TestClient_307JSONRedirectToJSON pins case A: JSON 307 points at
// ANOTHER _next/data JSON URL — the client follows it and parses.
func TestClient_307JSONRedirectToJSON(t *testing.T) {
	fixture := mustReadFixture(t, "event_page.json")
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/event/"):
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-1"}</script>`))
		case r.URL.Path == "/_next/data/build-1/en/event/x.json":
			w.Header().Set("Location", "/_next/data/build-1/en/event/x-v2.json?slug=x")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case r.URL.Path == "/_next/data/build-1/en/event/x-v2.json":
			seenPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pl, err := c.FetchEventPage(context.Background(), "x")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pl == nil || pl.Event.Title == "" {
		t.Fatalf("payload empty: %+v", pl)
	}
	if seenPath != "/_next/data/build-1/en/event/x-v2.json" {
		t.Errorf("expected follow-on JSON URL hit; got %q", seenPath)
	}
}

// TestClient_307JSONRedirectToHTMLSameSlug pins case B (canonical
// v10.5 hot path): JSON 307 points at /event/<same-slug>; client
// falls back to parsing __NEXT_DATA__ from the HTML page.
func TestClient_307JSONRedirectToHTMLSameSlug(t *testing.T) {
	// Embed the same fixture inside __NEXT_DATA__'s `props` wrapper
	// so the HTML fallback feeds the parser a payload it understands.
	fixture := mustReadFixture(t, "event_page.json")
	htmlPage := buildHTMLWithNextData(t, fixture, "build-X")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/_next/data/"):
			w.Header().Set("X-Nextjs-Redirect", "/event/cyprus-house-of-representatives-election-winner?slug=cyprus-house-of-representatives-election-winner")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, "/event/"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(htmlPage)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pl, err := c.FetchEventPage(context.Background(), "cyprus-house-of-representatives-election-winner")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pl == nil || pl.Event.Title == "" {
		t.Fatalf("HTML fallback must yield a parsed payload; got %+v err=%v", pl, err)
	}
	if pl.BuildID != "build-X" {
		t.Errorf("HTML fallback should propagate buildId from __NEXT_DATA__; got %q", pl.BuildID)
	}
}

// TestClient_307RedirectToCanonicalSlug pins case C: redirect target
// is a DIFFERENT /event/<slug>; client surfaces RedirectToCanonical
// + retries the JSON path with the canonical slug.
func TestClient_307RedirectToCanonicalSlug(t *testing.T) {
	fixture := mustReadFixture(t, "event_page.json")
	var canonicalHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/event/old-slug":
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-Z"}</script>`))
		case r.URL.Path == "/_next/data/build-Z/en/event/old-slug.json":
			w.Header().Set("Location", "/event/canonical-slug")
			w.WriteHeader(http.StatusMovedPermanently)
		case r.URL.Path == "/_next/data/build-Z/en/event/canonical-slug.json":
			canonicalHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		case strings.HasPrefix(r.URL.Path, "/event/"):
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-Z"}</script>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pl, err := c.FetchEventPage(context.Background(), "old-slug")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pl == nil || canonicalHits != 1 {
		t.Fatalf("expected canonical slug retry; got hits=%d pl=%+v", canonicalHits, pl)
	}
	// Second fetch on the original slug should hit the cached alias
	// directly — no redirect bounce.
	canonicalHits = 0
	if _, err := c.FetchEventPage(context.Background(), "old-slug"); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if canonicalHits != 1 {
		t.Errorf("alias cache should drive the second fetch straight to canonical JSON; got hits=%d", canonicalHits)
	}
}

// TestClient_RedirectLoopReturnsStructuredError pins case D: when
// the redirect chain exceeds MaxRedirects, the client surfaces
// CategoryRedirectLoop with the chain metadata.
func TestClient_RedirectLoopReturnsStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/event/"):
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-Q"}</script>`))
		case strings.HasPrefix(r.URL.Path, "/_next/data/"):
			// Every JSON URL bounces back to another JSON URL.
			w.Header().Set("Location", "/_next/data/build-Q/en/event/loop.json?slug=loop&hop="+r.URL.Query().Get("hop"))
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver, MaxRedirects: 2})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.FetchEventPage(context.Background(), "loop")
	fe, ok := AsFetchError(err)
	if !ok {
		t.Fatalf("expected *FetchError; got %T %v", err, err)
	}
	if fe.Category != CategoryRedirectLoop {
		t.Errorf("category: got %q want %q", fe.Category, CategoryRedirectLoop)
	}
}

// TestClient_UnsupportedRedirectTarget pins case E: redirect goes to
// something we can't handle (e.g. an external host). Surfaces typed
// error.
func TestClient_UnsupportedRedirectTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/event/"):
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-W"}</script>`))
		case strings.HasPrefix(r.URL.Path, "/_next/data/"):
			w.Header().Set("Location", "/some/totally-unrelated-path")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	}))
	defer srv.Close()
	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.FetchEventPage(context.Background(), "x")
	fe, ok := AsFetchError(err)
	if !ok {
		t.Fatalf("expected *FetchError; got %T %v", err, err)
	}
	if fe.Category != CategoryJSONContentTypeUnexpected {
		t.Errorf("category: got %q want %q", fe.Category, CategoryJSONContentTypeUnexpected)
	}
}

// TestClient_NextDataHTMLFallbackUnwrapsProps confirms the helper
// reproduces the JSON the parser expects.
func TestClient_NextDataHTMLFallbackUnwrapsProps(t *testing.T) {
	inner := `{"pageProps":{"dehydratedState":{"queries":[]}}}`
	html := []byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-Y","props":` + inner + `}</script>`)
	got, build := extractNextDataJSON(html)
	if build != "build-Y" {
		t.Errorf("buildId: got %q want build-Y", build)
	}
	if !strings.Contains(string(got), `"pageProps"`) {
		t.Errorf("unwrapped JSON missing pageProps: %s", got)
	}
	if strings.Contains(string(got), `"props":`) {
		t.Errorf("unwrap should strip the outer props envelope: %s", got)
	}
}

// TestClient_AliasStorePromotesAcrossInstances pins the persistence
// hand-off: when an alias is stashed via an AliasStore, a brand-new
// Client picks it up on the next fetch.
func TestClient_AliasStorePromotesAcrossInstances(t *testing.T) {
	fixture := mustReadFixture(t, "event_page.json")
	store := newMemoryAliasStore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/event/"):
			_, _ = w.Write([]byte(`<script id="__NEXT_DATA__" type="application/json">{"buildId":"build-K"}</script>`))
		case r.URL.Path == "/_next/data/build-K/en/event/canonical-x.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Pre-stash an alias.
	_ = store.UpsertEventSlugAlias(context.Background(), "old-x", "canonical-x", "test")

	resolver := NewBuildIDResolver(BuildIDResolverConfig{HTMLBaseURL: srv.URL, TTL: time.Hour})
	c, err := NewClient(ClientConfig{HTMLBaseURL: srv.URL, Resolver: resolver, AliasStore: store})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pl, err := c.FetchEventPage(context.Background(), "old-x")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pl == nil {
		t.Fatal("expected payload")
	}
}

// memoryAliasStore is the in-test AliasStore.
type memoryAliasStore struct {
	mu      sync.Mutex
	entries map[string]string
}

func newMemoryAliasStore() *memoryAliasStore { return &memoryAliasStore{entries: map[string]string{}} }

func (m *memoryAliasStore) UpsertEventSlugAlias(_ context.Context, original, canonical, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[original] = canonical
	return nil
}

func (m *memoryAliasStore) GetEventSlugAlias(_ context.Context, original string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.entries[original]
	return c, ok, nil
}

func buildHTMLWithNextData(t *testing.T, fixtureJSON []byte, buildID string) []byte {
	t.Helper()
	// Wrap the fixture (which is the data-route envelope) inside a
	// __NEXT_DATA__ JSON with `props: <fixture>`.
	return []byte(`<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"buildId":"` + buildID + `","props":` + string(fixtureJSON) + `}` +
		`</script></html>`)
}

// --- annotation hash dedup ------------------------------------------------

func TestAnnotationHash_StableForSameLogicalItem(t *testing.T) {
	a := EventAnnotation{
		EventSlug: "tx",
		UnixTime:  1778421600,
		Outcome:   "Ken Paxton",
		Title:     "Final pre-runoff poll shows Paxton leading with 63%",
	}
	b := a
	// Polymarket back-fills timestamps; the hash MUST NOT shift.
	b.Timestamp = b.Timestamp.Add(7 * time.Second)
	if AnnotationHash(a) != AnnotationHash(b) {
		t.Errorf("hash must be stable across timestamp jitter: %s vs %s", AnnotationHash(a), AnnotationHash(b))
	}
}

func TestAnnotationHash_DistinguishesDifferentItems(t *testing.T) {
	a := EventAnnotation{EventSlug: "tx", UnixTime: 1, Outcome: "x", Title: "a"}
	b := EventAnnotation{EventSlug: "tx", UnixTime: 1, Outcome: "x", Title: "b"}
	if AnnotationHash(a) == AnnotationHash(b) {
		t.Errorf("hash must change with title")
	}
}

// --- helpers --------------------------------------------------------------

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", p, err)
	}
	return b
}

// Compile-time assertion that *Client satisfies the seam.
var _ EventPageClient = (*Client)(nil)

// Unused but linked-in to keep the test binary tidy in case future
// tests need a singleflight-style assertion against the resolver.
var _ sync.Mutex
