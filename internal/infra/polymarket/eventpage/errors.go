// errors.go — typed failure categories for the eventpage fetch path.
//
// v10.5 replaces the previous generic `fmt.Errorf("eventpage: json
// status 307: ...")` pattern with a stable enum so the operator + the
// fallback path can route on category rather than parsing message
// strings. Polymarket's Next.js data route emits a 307 for every
// event slug today (with `x-nextjs-redirect: /event/<slug>?slug=...`
// pointing at the HTML page) — that is the canonical hot path, NOT
// a fatal error. Distinguishing it from real failures is the
// load-bearing change here.
package eventpage

import (
	"errors"
	"fmt"
)

// FetchErrorCategory is a small, stable string set that downstream
// code groups eventpage failures by. Each category maps cleanly to a
// metric label and (where relevant) a structured log line.
type FetchErrorCategory string

const (
	// CategoryRedirectFollowed — a redirect was followed to the
	// final document. Not a failure on its own; surfaces in metrics
	// so an operator can see the canonical-vs-redirect ratio.
	CategoryRedirectFollowed FetchErrorCategory = "redirect_followed"

	// CategoryRedirectLoop — we exceeded MaxRedirects, or the chain
	// re-entered a prior URL.
	CategoryRedirectLoop FetchErrorCategory = "redirect_loop"

	// CategoryRedirectToHTML — the JSON data route redirected to the
	// HTML page (the v10.5 canonical hot path). Indicates we should
	// fall back to parsing __NEXT_DATA__ inline; surfaces here only
	// when the HTML fallback ALSO failed.
	CategoryRedirectToHTML FetchErrorCategory = "redirect_to_html"

	// CategoryRedirectToCanonicalSlug — the redirect target points
	// at a different /event/<slug> than we asked for. Surfaced so
	// downstream code can stash the alias.
	CategoryRedirectToCanonicalSlug FetchErrorCategory = "redirect_to_canonical_slug"

	// CategoryStaleBuildID — JSON 404 + post-refresh JSON also 404.
	CategoryStaleBuildID FetchErrorCategory = "stale_build_id"

	// CategoryJSONStatusNon200 — JSON HTTP response was non-2xx and
	// not one of the canonical redirects we handle.
	CategoryJSONStatusNon200 FetchErrorCategory = "json_status_non_200"

	// CategoryJSONContentTypeUnexpected — body wasn't JSON (e.g. CDN
	// error page in HTML).
	CategoryJSONContentTypeUnexpected FetchErrorCategory = "json_content_type_unexpected"

	// CategoryJSONParseFailed — JSON decoded but the parsing step
	// returned an error.
	CategoryJSONParseFailed FetchErrorCategory = "json_parse_failed"

	// CategoryCanonicalSlugFailed — HTML fallback couldn't pull a
	// canonical slug or __NEXT_DATA__ payload.
	CategoryCanonicalSlugFailed FetchErrorCategory = "canonical_slug_failed"

	// CategoryNetworkTimeout — network error / context deadline.
	CategoryNetworkTimeout FetchErrorCategory = "network_timeout"

	// CategoryUnknown — fallthrough.
	CategoryUnknown FetchErrorCategory = "unknown"
)

// FetchError is the typed error returned by Client.FetchEventPage on
// every non-success path. Operators get a stable Category + concise
// metadata; the caller routes on Category, not message strings.
type FetchError struct {
	Category      FetchErrorCategory
	OriginalSlug  string
	CanonicalSlug string // populated when a redirect indicated a different canonical slug
	BuildID       string
	Status        int
	Location      string
	FinalURL      string
	ContentType   string
	RetryCount    int
	BodyPreview   string // ≤ 300 chars
	Underlying    error
}

func (e *FetchError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{"eventpage: " + string(e.Category)}
	if e.OriginalSlug != "" {
		parts = append(parts, "slug="+e.OriginalSlug)
	}
	if e.CanonicalSlug != "" && e.CanonicalSlug != e.OriginalSlug {
		parts = append(parts, "canonical="+e.CanonicalSlug)
	}
	if e.BuildID != "" {
		parts = append(parts, "build_id="+e.BuildID)
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.Status))
	}
	if e.Location != "" {
		parts = append(parts, "location="+e.Location)
	}
	if e.ContentType != "" {
		parts = append(parts, "content_type="+e.ContentType)
	}
	if e.RetryCount > 0 {
		parts = append(parts, fmt.Sprintf("retries=%d", e.RetryCount))
	}
	if e.Underlying != nil {
		parts = append(parts, "err="+e.Underlying.Error())
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}

func (e *FetchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Underlying
}

// AsFetchError unwraps a *FetchError from any error chain.
func AsFetchError(err error) (*FetchError, bool) {
	if err == nil {
		return nil, false
	}
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}
