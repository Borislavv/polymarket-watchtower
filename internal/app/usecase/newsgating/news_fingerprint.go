// Package newsgating implements the v10.7 news-driven AI gating
// layer. The premise: AI should run only when meaningful information
// changed. Primary trigger is the event-page annotation set
// (Polymarket Next.js event endpoint). Secondary triggers
// (significant price move, new p99/p99.5 alert, catalyst status
// change, repricing status change) are accepted on the call site.
//
// The package is intentionally tiny — fingerprint computation is
// pure, persistence sits behind the repository.NewsFingerprintRepository
// interface, and the gating decision is one boolean function the
// orchestrator calls before paying for an AI request.
package newsgating

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Annotation is the small projection the fingerprint hashes. The
// caller passes whatever shape it has; only the load-bearing fields
// land in the hash.
type Annotation struct {
	ItemHash    string
	Title       string
	Outcome     string
	Timestamp   time.Time
	PriceBefore float64
	PriceAfter  float64
	PriceChange float64
	SourceName  string
	// Sources is the dedup-friendly list of source names + URLs as
	// rendered by the v10.5 marketintel surface. Order-independent.
	Sources []SourceRef
}

// SourceRef is one annotation citation.
type SourceRef struct {
	Name string
	URL  string
}

// Compute produces the stable fingerprint hash + summary counters for
// a set of annotations belonging to one event_slug. The hash inputs
// are sorted so re-ordering noise from the upstream API doesn't flip
// the fingerprint.
//
// Hash inputs (per annotation, joined by "|"):
//
//	item_hash, title, timestamp (unix), outcome, price_before,
//	price_after, price_change, source_name, sorted (name+URL) tuples.
//
// Hash inputs the function deliberately EXCLUDES:
//
//	now / fetch_time / wall-clock — would flip the hash on every call.
//	annotation array index — would flip on re-ordering.
//	any DB-side jitter (created_at / first_seen_at / last_seen_at).
func Compute(eventSlug string, annotations []Annotation) (fingerprint string, annotationCount int32, latestAnnotationAt time.Time) {
	rows := make([]string, 0, len(annotations))
	for _, a := range annotations {
		// Sources order-independent.
		srcs := make([]string, 0, len(a.Sources))
		for _, s := range a.Sources {
			srcs = append(srcs, strings.TrimSpace(s.Name)+":"+strings.TrimSpace(s.URL))
		}
		sort.Strings(srcs)
		row := fmt.Sprintf("%s|%s|%d|%s|%.4f|%.4f|%.4f|%s|%s",
			strings.TrimSpace(a.ItemHash),
			normalize(a.Title),
			a.Timestamp.UTC().Unix(),
			normalize(a.Outcome),
			a.PriceBefore, a.PriceAfter, a.PriceChange,
			normalize(a.SourceName),
			strings.Join(srcs, ","),
		)
		rows = append(rows, row)
		if a.Timestamp.After(latestAnnotationAt) {
			latestAnnotationAt = a.Timestamp
		}
	}
	sort.Strings(rows)
	body := strings.TrimSpace(eventSlug) + "::" + strings.Join(rows, "\n")
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:]), int32(len(annotations)), latestAnnotationAt
}

// Decision is the gating result. Allow=true means the surface MAY
// call AI; Allow=false means it MUST NOT (unless a secondary trigger
// the caller knows about overrides).
type Decision struct {
	Allow               bool
	Reason              string // "news_changed" | "stale_no_record" | "unchanged"
	Fingerprint         string
	PreviousFingerprint string
	LastAICalledAt      time.Time
}

// Gate evaluates whether AI should be allowed for one event_slug
// given a freshly-computed fingerprint and the persisted state.
//
// Rules:
//  1. No persisted record yet → Allow (the AI is the source of truth
//     for the first observation).
//  2. Fingerprint matches the persisted one → DenyUnchanged.
//  3. Fingerprint differs → Allow (news_changed).
//
// Empty fingerprint inputs (e.g. zero annotations) still allow the
// FIRST call so the system can register the baseline.
func Gate(prev repository.NewsFingerprint, prevFound bool, newFingerprint string) Decision {
	if !prevFound {
		return Decision{Allow: true, Reason: "stale_no_record", Fingerprint: newFingerprint}
	}
	if prev.Fingerprint == newFingerprint {
		return Decision{
			Allow:               false,
			Reason:              "unchanged",
			Fingerprint:         newFingerprint,
			PreviousFingerprint: prev.Fingerprint,
			LastAICalledAt:      prev.LastAICalledAt,
		}
	}
	return Decision{
		Allow:               true,
		Reason:              "news_changed",
		Fingerprint:         newFingerprint,
		PreviousFingerprint: prev.Fingerprint,
		LastAICalledAt:      prev.LastAICalledAt,
	}
}

// SemanticDecision is the dedupe result for the OUTPUT (after the AI
// has run). Allow=true means the surface MAY ship the Telegram body;
// Allow=false means an identical semantic fingerprint shipped inside
// the configured cooldown.
type SemanticDecision struct {
	Allow            bool
	Reason           string
	Fingerprint      string
	LastShippedAt    time.Time
	LastSemanticCode string
}

// SemanticGate evaluates "did we already ship the same conclusion
// recently?" — used to suppress repeated no-edge/already-priced
// reports under the configured cooldown.
//
// `cooldown` controls how long an identical conclusion is silenced.
func SemanticGate(prev repository.NewsFingerprint, prevFound bool, newSemanticFingerprint string, now time.Time, cooldown time.Duration) SemanticDecision {
	if !prevFound || prev.LastSemanticFingerprint == "" {
		return SemanticDecision{Allow: true, Reason: "no_prior_semantic", Fingerprint: newSemanticFingerprint}
	}
	if prev.LastSemanticFingerprint != newSemanticFingerprint {
		return SemanticDecision{Allow: true, Reason: "semantic_changed", Fingerprint: newSemanticFingerprint, LastShippedAt: prev.LastSemanticAt, LastSemanticCode: prev.LastSemanticCode}
	}
	if cooldown <= 0 || now.Sub(prev.LastSemanticAt) >= cooldown {
		return SemanticDecision{Allow: true, Reason: "cooldown_elapsed", Fingerprint: newSemanticFingerprint, LastShippedAt: prev.LastSemanticAt, LastSemanticCode: prev.LastSemanticCode}
	}
	return SemanticDecision{Allow: false, Reason: "cooldown_active", Fingerprint: newSemanticFingerprint, LastShippedAt: prev.LastSemanticAt, LastSemanticCode: prev.LastSemanticCode}
}

// ComputeSemantic produces the deterministic output-fingerprint. The
// caller passes the normalised conclusion fields; the function hashes
// in stable order and never includes `now`. Same shape as Compute.
func ComputeSemantic(reportType, regime, stance string, sentinelCode string, topEvents []string, topMarkets []string) string {
	sort.Strings(topEvents)
	sort.Strings(topMarkets)
	body := strings.Join([]string{
		strings.ToLower(reportType),
		strings.ToLower(regime),
		strings.ToLower(stance),
		strings.ToUpper(sentinelCode),
		strings.Join(topEvents, ","),
		strings.Join(topMarkets, ","),
	}, "::")
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
