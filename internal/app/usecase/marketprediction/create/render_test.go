package create

import (
	"strings"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// floatPtr is a tiny helper so the test literals stay legible.
func floatPtr(v float64) *float64 { return &v }

func sampleAnnotations() []repository.EventAnnotation {
	return []repository.EventAnnotation{
		{
			Timestamp:   time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
			Title:       "Ken Paxton announces Texas Senate primary bid after legal troubles",
			Outcome:     "Ken Paxton",
			PriceBefore: floatPtr(50),
			PriceAfter:  floatPtr(62),
			PriceChange: floatPtr(12),
			SourcesJSON: []byte(`[{"name":"AP News"},{"name":"Reuters"},{"name":"NY Times"},{"name":"WSJ"}]`),
		},
		{
			Timestamp:   time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
			Title:       "John Cornyn faces tough primary challenge from Paxton and Hunt",
			Outcome:     "John Cornyn",
			PriceBefore: floatPtr(50),
			PriceAfter:  floatPtr(34),
			PriceChange: floatPtr(-16),
			SourcesJSON: []byte(`[{"name":"AP News"}]`),
		},
	}
}

// PART 9 / TEST 1: prediction message renders Links block.
func TestRenderCreation_RendersLinksBlock(t *testing.T) {
	body := RenderCreationTelegram(CreationRenderInput{
		EventSlug:           "texas-senate",
		Question:            "Will X win?",
		Outcome:             "Ken Paxton",
		SideBias:            "bullish",
		Confidence:          0.82,
		Summary:             "Thesis body sufficient to ship.",
		PolymarketEventURL:  "https://polymarket.com/event/texas-senate",
		PolymarketMarketURL: "https://polymarket.com/event/texas-senate?market=0xabc",
		GrafanaURL:          "https://grafana.example.com/d/uid?from=now-1h",
	})
	if !strings.Contains(body, "<b>Links</b>") {
		t.Fatalf("missing Links header in body:\n%s", body)
	}
	for _, want := range []string{
		`<a href="https://polymarket.com/event/texas-senate">Polymarket event</a>`,
		`<a href="https://grafana.example.com/d/uid?from=now-1h">Grafana</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Links block missing %q in body:\n%s", want, body)
		}
	}
}

// PART 9 / TEST 2: prediction message renders latest annotations.
func TestRenderCreation_RendersLatestAnnotations(t *testing.T) {
	body := RenderCreationTelegram(CreationRenderInput{
		EventSlug:                "tx",
		Summary:                  "x",
		Annotations:              sampleAnnotations(),
		MaxAnnotationTitleChars:  160,
		MaxAnnotationSourceNames: 3,
	})
	if !strings.Contains(body, "<b>Latest Polymarket events</b>") {
		t.Fatal("missing Latest Polymarket events header")
	}
	// Row 1 header line + truncated title + sources line.
	if !strings.Contains(body, "1. 2026-05-10 · Ken Paxton · 50→62 (+12)") {
		t.Errorf("row 1 header malformed: %q", snippet(body))
	}
	if !strings.Contains(body, "sources: AP News, Reuters, NY Times") {
		t.Errorf("row 1 sources cap of 3 not applied: %q", snippet(body))
	}
	if strings.Contains(body, "WSJ") {
		t.Error("4th source should have been dropped by MaxAnnotationSourceNames=3")
	}
	if !strings.Contains(body, "2. 2026-05-11 · John Cornyn · 50→34 (-16)") {
		t.Errorf("row 2 header malformed: %q", snippet(body))
	}
}

// PART 9 / TEST 3: annotations block omitted when empty.
func TestRenderCreation_AnnotationsBlockOmittedWhenEmpty(t *testing.T) {
	body := RenderCreationTelegram(CreationRenderInput{
		EventSlug: "tx",
		Summary:   "x",
	})
	if strings.Contains(body, "Latest Polymarket events") {
		t.Errorf("expected annotation block omitted; body=%q", body)
	}
}

// PART 9 / TEST 4: unsafe links rejected (localhost / javascript:).
func TestRenderCreation_UnsafeLinksRejected(t *testing.T) {
	body := RenderCreationTelegram(CreationRenderInput{
		EventSlug:           "tx",
		Summary:             "x",
		PolymarketEventURL:  "javascript:alert(1)",
		PolymarketMarketURL: "https://polymarket.com/event/ok",
		GrafanaURL:          "http://localhost:3000/d/uid",
	})
	if strings.Contains(body, "javascript:") {
		t.Error("javascript: URL must be sanitized away")
	}
	if strings.Contains(body, "localhost:3000") {
		t.Error("localhost URL must be sanitized away")
	}
	if !strings.Contains(body, `href="https://polymarket.com/event/ok"`) {
		t.Errorf("safe URL should remain: %s", snippet(body))
	}
}

// PART 9 / TEST 5: no orphan link labels — when every URL is unsafe,
// the entire Links section elides.
func TestRenderCreation_NoOrphanLinkLabels(t *testing.T) {
	body := RenderCreationTelegram(CreationRenderInput{
		EventSlug:          "tx",
		Summary:            "x",
		PolymarketEventURL: "http://localhost/event",
		GrafanaURL:         "http://127.0.0.1/d/x",
	})
	if strings.Contains(body, "Links") {
		t.Errorf("Links header should be elided when all URLs unsafe; body=%q", body)
	}
	// Defensive: no raw label text should leak.
	for _, label := range []string{"Polymarket event", "Polymarket market", "Grafana", "Trader"} {
		if strings.Contains(body, "• "+label) {
			t.Errorf("orphan label leak: %q", label)
		}
	}
}

// PART 9 / TEST 6: body split remains valid HTML — the safe-splitter
// keeps tag pairs balanced even when a chunk would otherwise straddle
// a bold span. (Exercises the alerting package via SafeSplitForTelegram.)
func TestRenderCreation_BodySplitKeepsHTMLValid(t *testing.T) {
	long := strings.Repeat("paragraph A. ", 200) + "\n\n<b>" + strings.Repeat("X ", 1500) + "</b>"
	parts := alerting.SafeSplitForTelegram(long)
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks; got %d", len(parts))
	}
	for i, p := range parts {
		if strings.Count(p, "<b>") != strings.Count(p, "</b>") {
			t.Errorf("chunk %d has unbalanced <b>/</b>: %q", i, snippet(p))
		}
		if len(p) > alerting.TelegramMaxMessageChars {
			t.Errorf("chunk %d exceeds cap: %d > %d", i, len(p), alerting.TelegramMaxMessageChars)
		}
	}
}

// snippet keeps test output legible when assertions fail on long bodies.
func snippet(s string) string {
	if len(s) <= 240 {
		return s
	}
	return s[:120] + "…" + s[len(s)-120:]
}
