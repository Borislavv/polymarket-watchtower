package newsgating

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// PART 17 test 1: unchanged news skips AI.
func TestGate_UnchangedNewsSkipsAI(t *testing.T) {
	prev := repository.NewsFingerprint{Fingerprint: "abc"}
	d := Gate(prev, true, "abc")
	if d.Allow {
		t.Errorf("identical fingerprint must skip AI; got %+v", d)
	}
}

// PART 17 test 2: changed news allows AI.
func TestGate_ChangedNewsAllowsAI(t *testing.T) {
	prev := repository.NewsFingerprint{Fingerprint: "abc"}
	d := Gate(prev, true, "def")
	if !d.Allow {
		t.Errorf("different fingerprint must allow AI; got %+v", d)
	}
	if d.Reason != "news_changed" {
		t.Errorf("reason: got %q want news_changed", d.Reason)
	}
}

// First-ever observation: no persisted row → allow.
func TestGate_NoPriorRecordAllows(t *testing.T) {
	d := Gate(repository.NewsFingerprint{}, false, "abc")
	if !d.Allow {
		t.Errorf("first-ever observation must allow AI; got %+v", d)
	}
}

// PART 4 invariant: fingerprint is order-independent.
func TestCompute_OrderIndependent(t *testing.T) {
	t1 := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC)
	a := []Annotation{
		{ItemHash: "h1", Title: "A", Timestamp: t1, PriceBefore: 0.5, PriceAfter: 0.6, SourceName: "AP"},
		{ItemHash: "h2", Title: "B", Timestamp: t2, PriceBefore: 0.4, PriceAfter: 0.5, SourceName: "Reuters"},
	}
	b := []Annotation{
		{ItemHash: "h2", Title: "B", Timestamp: t2, PriceBefore: 0.4, PriceAfter: 0.5, SourceName: "Reuters"},
		{ItemHash: "h1", Title: "A", Timestamp: t1, PriceBefore: 0.5, PriceAfter: 0.6, SourceName: "AP"},
	}
	fa, _, _ := Compute("x", a)
	fb, _, _ := Compute("x", b)
	if fa != fb {
		t.Errorf("fingerprint must be order-independent:\n%s\n%s", fa, fb)
	}
}

// PART 4 invariant: changing one annotation field flips the fingerprint.
func TestCompute_TitleChangeFlipsFingerprint(t *testing.T) {
	a := []Annotation{{ItemHash: "h1", Title: "A", PriceBefore: 0.5}}
	b := []Annotation{{ItemHash: "h1", Title: "A_v2", PriceBefore: 0.5}}
	fa, _, _ := Compute("x", a)
	fb, _, _ := Compute("x", b)
	if fa == fb {
		t.Errorf("title change must flip fingerprint")
	}
}

// PART 4 invariant: now() is NOT in the hash.
func TestCompute_DoesNotIncludeWallclock(t *testing.T) {
	a := []Annotation{{ItemHash: "h1", Title: "A", Timestamp: time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)}}
	fa, _, _ := Compute("x", a)
	// Same input again at "different time"; the function takes no
	// time.Now parameter — proves wall-clock is not embedded.
	fb, _, _ := Compute("x", a)
	if fa != fb {
		t.Errorf("repeat compute on identical input must yield identical hash")
	}
}

// PART 5 semantic gate: identical conclusion inside cooldown suppressed.
func TestSemanticGate_DuplicateInCooldownSuppressed(t *testing.T) {
	prev := repository.NewsFingerprint{
		LastSemanticFingerprint: "sem-abc",
		LastSemanticAt:          time.Now().Add(-1 * time.Hour),
	}
	d := SemanticGate(prev, true, "sem-abc", time.Now(), 12*time.Hour)
	if d.Allow {
		t.Errorf("identical semantic fingerprint in cooldown must suppress; got %+v", d)
	}
}

// Cooldown elapsed → allow.
func TestSemanticGate_CooldownElapsedAllows(t *testing.T) {
	prev := repository.NewsFingerprint{
		LastSemanticFingerprint: "sem-abc",
		LastSemanticAt:          time.Now().Add(-25 * time.Hour),
	}
	d := SemanticGate(prev, true, "sem-abc", time.Now(), 12*time.Hour)
	if !d.Allow {
		t.Errorf("cooldown elapsed must allow; got %+v", d)
	}
}

// Different conclusion → allow.
func TestSemanticGate_DifferentSemanticAllows(t *testing.T) {
	prev := repository.NewsFingerprint{
		LastSemanticFingerprint: "sem-abc",
		LastSemanticAt:          time.Now(),
	}
	d := SemanticGate(prev, true, "sem-xyz", time.Now(), 12*time.Hour)
	if !d.Allow {
		t.Errorf("different semantic fingerprint must allow; got %+v", d)
	}
}

// ComputeSemantic is order-independent across topEvents / topMarkets.
func TestComputeSemantic_OrderIndependent(t *testing.T) {
	a := ComputeSemantic("market_intel", "news_changed", "actionable", "", []string{"a", "b"}, []string{"m1", "m2"})
	b := ComputeSemantic("market_intel", "news_changed", "actionable", "", []string{"b", "a"}, []string{"m2", "m1"})
	if a != b {
		t.Errorf("order independence broken: %s vs %s", a, b)
	}
}
