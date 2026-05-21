package alerting

import (
	"strings"
	"testing"
	"time"
)

func TestBuildPolymarketEventURL(t *testing.T) {
	if got := BuildPolymarketEventURL("https://polymarket.com", "x-event"); got != "https://polymarket.com/event/x-event" {
		t.Errorf("happy path: %q", got)
	}
	if got := BuildPolymarketEventURL("https://polymarket.com", ""); got != "" {
		t.Errorf("empty slug must return empty: %q", got)
	}
	if got := BuildPolymarketEventURL("", "x"); got != "" {
		t.Errorf("empty base must return empty: %q", got)
	}
	// Loopback should be rejected even when slug is valid.
	if got := BuildPolymarketEventURL("http://localhost:3000", "x"); got != "" {
		t.Errorf("localhost base must elide: %q", got)
	}
}

func TestBuildPolymarketMarketURL_FallsBackToConditionID(t *testing.T) {
	if got := BuildPolymarketMarketURL("https://polymarket.com", "", "0xabc"); got != "https://polymarket.com/markets/0xabc" {
		t.Errorf("fallback to conditionID: %q", got)
	}
}

func TestBuildGrafanaURL_ElidesOnMissingConfig(t *testing.T) {
	if got := BuildGrafanaURL(LinksInput{GrafanaDashUID: "uid"}); got != "" {
		t.Errorf("missing base must elide: %q", got)
	}
	if got := BuildGrafanaURL(LinksInput{GrafanaBase: "https://g.example.com"}); got != "" {
		t.Errorf("missing dashUID must elide: %q", got)
	}
}

func TestRenderLinksBlock_NoOrphanHeader(t *testing.T) {
	// No base URLs configured → every entry elides → no orphan
	// "Links" header.
	if got := RenderLinksBlock(LinksInput{EventSlug: "x", MarketSlug: "y"}); got != "" {
		t.Errorf("expected empty output (no Links header) when no base config; got:\n%s", got)
	}
}

func TestRenderLinksBlock_HappyPath(t *testing.T) {
	got := RenderLinksBlock(LinksInput{
		PolymarketBase: "https://polymarket.com",
		GrafanaBase:    "https://grafana.example.com",
		GrafanaDashUID: "uid-1",
		GrafanaContext: time.Hour,
		EventSlug:      "fed-cut",
		MarketSlug:     "fed-cut-25",
		CategorySlug:   "macro",
		Wallet:         "0xabc",
		Sources: []SourceURL{
			{Name: "AP", URL: "https://apnews.com/x"},
			{Name: "Reuters", URL: "https://reuters.com/x"},
		},
		At:       time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
		Severity: "warning",
	})
	for _, want := range []string{
		"<b>Links</b>",
		`<a href="https://polymarket.com/event/fed-cut">Polymarket event</a>`,
		`<a href="https://polymarket.com/markets/fed-cut-25">Market</a>`,
		`<a href="https://polymarket.com/predictions/macro">Category</a>`,
		`<a href="https://polymarket.com/profile/0xabc">Trader</a>`,
		`<a href="https://apnews.com/x">AP</a>`,
		`<a href="https://reuters.com/x">Reuters</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in body:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "var-severity=warning") {
		t.Errorf("Grafana var-severity missing")
	}
}

func TestRenderLinksBlock_DropsUnsafeURLs(t *testing.T) {
	got := RenderLinksBlock(LinksInput{
		PolymarketBase: "https://polymarket.com",
		EventSlug:      "x",
		Sources: []SourceURL{
			{Name: "AP", URL: "https://apnews.com/x"},
			{Name: "localhost", URL: "http://localhost:8080/x"},
			{Name: "javascript", URL: "javascript:alert(1)"},
		},
	})
	if strings.Contains(got, "localhost") || strings.Contains(got, "javascript:") {
		t.Errorf("unsafe URLs must be dropped:\n%s", got)
	}
	if !strings.Contains(got, `<a href="https://apnews.com/x">AP</a>`) {
		t.Errorf("safe URL must remain:\n%s", got)
	}
}

func TestRenderLinksBlock_RespectsMaxLinks(t *testing.T) {
	got := RenderLinksBlock(LinksInput{
		PolymarketBase: "https://polymarket.com",
		EventSlug:      "x",
		MarketSlug:     "y",
		CategorySlug:   "z",
		Wallet:         "0xw",
		MaxLinks:       2,
	})
	// Expect Polymarket event + Market only.
	if !strings.Contains(got, "Polymarket event") {
		t.Errorf("first link missing")
	}
	if !strings.Contains(got, ">Market</a>") {
		t.Errorf("second link missing")
	}
	if strings.Contains(got, ">Category</a>") || strings.Contains(got, ">Trader</a>") {
		t.Errorf("MaxLinks must cap to 2:\n%s", got)
	}
}
