package alerting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
)

func sampleTradeFinding() anomaly.Finding {
	return anomaly.Finding{
		Kind:     anomaly.KindTradeAnomaly,
		Severity: anomaly.SeverityCritical,
		Reason:   anomaly.ReasonSingle,
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Trade: &anomaly.TradeRef{
			ID:          "trade-1",
			Wallet:      "0xabc1234567890def1234567890abcdef12345678",
			Market:      "0xabc",
			Slug:        "rain-tomorrow",
			Question:    "Will it rain <tomorrow>?", // exercise HTML escaping
			Outcome:     "Yes",
			Side:        trade.SideBuy,
			SizeShares:  4_000,
			Price:       0.05,
			Odds:        20,
			NotionalUSD: 120_000,
			At:          time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		},
		Baseline: &anomaly.BaselineRef{
			Scope:     "category=Weather market=rain outcome=Yes",
			MedianUSD: 9.70, MeanUSD: 12.10, P95USD: 60, SampleN: 1240,
			Span:      30*24*time.Hour + 6*time.Hour,
			WindowMax: 365 * 24 * time.Hour,
		},
		Category:         &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather & Climate"},
		Multiplier:       12_371,
		AbsoluteTier:     anomaly.SeverityCritical,
		MultiplierTier:   anomaly.SeverityCritical,
		MarketURL:        "https://polymarket.com/event/rain-tomorrow",
		CategoryURL:      "https://polymarket.com/predictions/weather",
		TraderURL:        "https://polymarket.com/profile/0xabc1234567890def1234567890abcdef12345678",
		GrafanaURL:       "http://grafana.public/d/uid123/?from=1&to=2&var-category=Weather&var-market=rain-tomorrow&var-severity=critical",
		LifecyclePct:     93.5,
		Hot:              true,
		InCluster:        true,
		ClusterPeerCount: 4,
	}
}

func sampleClusterFinding() anomaly.Finding {
	return anomaly.Finding{
		Kind:     anomaly.KindCategoryWatch,
		Severity: anomaly.SeverityHard,
		Reason:   anomaly.ReasonCluster,
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Category: &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather"},
		Cluster: &anomaly.ClusterStats{
			Window: 30 * time.Minute, AnomalousTrades: 4, UniqueWallets: 3, TotalUSD: 184_000,
			Sample: []anomaly.TradeRef{
				{Question: "Will it rain?", NotionalUSD: 50_000, Outcome: "Yes", Wallet: "0xabc1234567890def1234567890abcdef12345678"},
				{Question: "Snow on Friday?", NotionalUSD: 40_000, Outcome: "No", Wallet: "0xfeed4567890abc1234567890abcdef12345678ab"},
			},
		},
		MarketURL:  "https://polymarket.com/event/rain-tomorrow",
		GrafanaURL: "http://grafana.local/d/uid123/?var-category=Weather&var-severity=hard",
	}
}

func TestTelegramDisabledIsNoop(t *testing.T) {
	s, err := NewTelegramSink(TelegramConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("disabled sink returned %v", err)
	}
}

// TestTelegramEnabledRequiresTokenAndChatID locks in the fail-fast contract:
// an enabled sink without both bot token and chat id must return an error
// at construction so misconfiguration is visible at startup, not silently
// at the first alert.
func TestTelegramEnabledRequiresTokenAndChatID(t *testing.T) {
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true}); err == nil {
		t.Fatal("expected error when bot token missing")
	}
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t"}); err == nil {
		t.Fatal("expected error when chat id missing")
	}
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", ChatID: "1"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestTradeAnomalyHeaderFormat(t *testing.T) {
	msg := FormatTelegramMessage(sampleTradeFinding())
	first := strings.SplitN(msg, "\n", 2)[0]
	// Header: <b>SEV: xMUL · $NOTIONAL · HOT · TITLE</b>
	for _, want := range []string{"<b>", "CRITICAL", "x12371", "$120,000", "HOT", "Will it rain &lt;tomorrow&gt;?", "</b>"} {
		if !strings.Contains(first, want) {
			t.Errorf("header missing %q in:\n%s", want, first)
		}
	}
}

func TestTradeAnomalyMessageHasAllRequiredSections(t *testing.T) {
	msg := FormatTelegramMessage(sampleTradeFinding())
	for _, want := range []string{
		"<b>Why</b>",
		"<b>x12371</b> above baseline median ($9.70)",
		"odds <b>20.0</b>, implied probability <b>5.0%</b>",
		"baseline: <b>1240</b> trades, median $9.70",
		"span 30d6h of available history",
		"tiers: absolute=<code>critical</code> multiplier=<code>critical</code>",
		"market lifecycle: <b>93.5%</b> elapsed (HOT — final stretch)",
		"<b>part of a forming cluster</b>: 4 anomalous trades",
		"<b>Trade</b>",
		"outcome: <b>Yes</b> (BUY)",
		"size: $120,000",
		"trader: <code>0xabc1234567890def1234567890abcdef12345678</code>",
		"category: Weather &amp; Climate",
		"<b>Links</b>",
		`• <a href="https://polymarket.com/event/rain-tomorrow">Polymarket market</a>`,
		`• <a href="https://polymarket.com/predictions/weather">Polymarket category</a>`,
		`• <a href="https://polymarket.com/profile/0xabc1234567890def1234567890abcdef12345678">Trader</a>`,
		`• <a href="http://grafana.public/d/uid123/?from=1&amp;to=2&amp;var-category=Weather&amp;var-market=rain-tomorrow&amp;var-severity=critical">Grafana</a>`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestSingleTradeAloneSaysSo(t *testing.T) {
	f := sampleTradeFinding()
	f.InCluster = false
	f.ClusterPeerCount = 1
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "single trade (no peers in cluster window yet)") {
		t.Errorf("missing single-trade hint in:\n%s", msg)
	}
}

func TestLinksOmittedWhenAllURLsEmpty(t *testing.T) {
	f := sampleTradeFinding()
	f.MarketURL, f.CategoryURL, f.TraderURL, f.GrafanaURL = "", "", "", ""
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "<b>Links</b>") {
		t.Errorf("Links section should be omitted entirely when no URLs are set:\n%s", msg)
	}
}

func TestLinksOmitMissingEntries(t *testing.T) {
	// Plain-text "Grafana" must NEVER appear — only as a hyperlink when URL is set.
	f := sampleTradeFinding()
	f.GrafanaURL = ""
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, ">Grafana</a>") || strings.Contains(msg, "• Grafana") {
		t.Errorf("Grafana entry must be hidden when GrafanaURL is empty:\n%s", msg)
	}
	// The other three should still be present.
	for _, want := range []string{"Polymarket market", "Polymarket category", "Trader"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestCategoryWatchMessageHasAllRequiredFields(t *testing.T) {
	msg := FormatTelegramMessage(sampleClusterFinding())
	for _, want := range []string{
		"<b>HARD — CategoryWatchRequired:",
		"4 trades · 3 wallets · $184,000 · Weather",
		"<b>Cluster</b>",
		"<b>4 anomalous trades</b>",
		"<b>3 unique traders</b>",
		"<b>$184,000 total anomalous notional</b>",
		"window: 30m",
		"<b>Recent contributors</b>",
		"Will it rain?",
		"Snow on Friday?",
		"$50,000",
		"$40,000",
		"<b>Links</b>",
		"Grafana",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

// TestTelegramHTMLParseMode captures the wire payload from a fake Telegram
// API server and asserts the contract: parse_mode=HTML, recipient is
// exactly the configured chat (numeric form for groups/private chats), no
// broadcast to anyone else.
func TestTelegramHTMLParseMode(t *testing.T) {
	var received atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received.Store(body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s, err := NewTelegramSink(TelegramConfig{
		Enabled: true, BotToken: "fake", ChatID: "1", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	raw, _ := received.Load().([]byte)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if body["parse_mode"] != "HTML" {
		t.Errorf("parse_mode: got %v want HTML", body["parse_mode"])
	}
	if got, _ := body["chat_id"].(float64); int64(got) != 1 {
		t.Errorf("chat_id: %v", body["chat_id"])
	}
}

// TestTelegramAcceptsChannelUsername confirms that a non-numeric chat id
// (e.g. "@watchtower-alerts") is forwarded verbatim as a string — Telegram
// supports both encodings on the same endpoint.
func TestTelegramAcceptsChannelUsername(t *testing.T) {
	var received atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received.Store(body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s, _ := NewTelegramSink(TelegramConfig{
		Enabled: true, BotToken: "t", ChatID: "@watchtower-alerts", BaseURL: srv.URL,
	})
	_ = s.Notify(context.Background(), sampleTradeFinding())
	raw, _ := received.Load().([]byte)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["chat_id"] != "@watchtower-alerts" {
		t.Errorf("non-numeric chat id must be forwarded as string, got %v (%T)",
			body["chat_id"], body["chat_id"])
	}
}

func TestTelegramReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	s, _ := NewTelegramSink(TelegramConfig{
		Enabled: true, BotToken: "t", ChatID: "1", BaseURL: srv.URL,
	})
	if err := s.Notify(context.Background(), sampleTradeFinding()); err == nil {
		t.Fatal("expected error for 400")
	}
}

// TestTelegramSendsExactlyOncePerNotify proves the sink does not fan out to
// multiple chats — the subscriber registry / getUpdates path was deleted on
// purpose. Multiple Notify calls produce multiple sends; one Notify, one send.
func TestTelegramSendsExactlyOncePerNotify(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	s, _ := NewTelegramSink(TelegramConfig{
		Enabled: true, BotToken: "t", ChatID: "42", BaseURL: srv.URL,
	})
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 send, got %d", got)
	}
}

func TestEscapingHandlesSpecialCharsInTitle(t *testing.T) {
	f := sampleTradeFinding()
	f.Trade.Question = `Will the price of "BTC" be > $100k & < $200k by 2026?`
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "&gt; $100k &amp; &lt;") {
		t.Errorf("HTML escaping missing in:\n%s", msg)
	}
}

// --- Link rendering contract ------------------------------------------------
// These tests pin the wire-format contract for the Links section. They are
// the line of defence against the bug where "Grafana" once rendered as
// plain text instead of a clickable anchor.

// TestRenderLinkBuildsHTMLAnchor pins the helper used everywhere link
// rendering happens. Anchor markup, href escaping, label escaping.
func TestRenderLinkBuildsHTMLAnchor(t *testing.T) {
	got := renderLink("Grafana", "http://grafana.public/d/uid/?a=1&b=2")
	want := `<a href="http://grafana.public/d/uid/?a=1&amp;b=2">Grafana</a>`
	if got != want {
		t.Fatalf("renderLink mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestRenderLinkEscapesLabel guards against future labels accidentally
// containing user-controlled text. The renderLink helper must escape both
// sides regardless of caller — Telegram silently drops a malformed entity
// and the link would degrade to plain text.
func TestRenderLinkEscapesLabel(t *testing.T) {
	got := renderLink("A <fancy> & rare label", "https://x.test/")
	want := `<a href="https://x.test/">A &lt;fancy&gt; &amp; rare label</a>`
	if got != want {
		t.Fatalf("renderLink label escape:\n got: %q\nwant: %q", got, want)
	}
}

// TestRenderLinkEmptyHrefReturnsPlainLabel documents the safety-net
// branch. Callers are still expected to skip empty hrefs upstream.
func TestRenderLinkEmptyHrefReturnsPlainLabel(t *testing.T) {
	if got := renderLink("Grafana", ""); got != "Grafana" {
		t.Fatalf("empty href: got %q want %q", got, "Grafana")
	}
}

// TestLinksSectionExactFormat pins the entire Links block byte-for-byte
// when all four URLs are present. If this test fails, the wire format
// changed and operators' alert templates / parsers may regress with it.
func TestLinksSectionExactFormat(t *testing.T) {
	msg := FormatTelegramMessage(sampleTradeFinding())
	const want = "\n<b>Links</b>\n" +
		`• <a href="https://polymarket.com/event/rain-tomorrow">Polymarket market</a>` + "\n" +
		`• <a href="https://polymarket.com/predictions/weather">Polymarket category</a>` + "\n" +
		`• <a href="https://polymarket.com/profile/0xabc1234567890def1234567890abcdef12345678">Trader</a>` + "\n" +
		`• <a href="http://grafana.public/d/uid123/?from=1&amp;to=2&amp;var-category=Weather&amp;var-market=rain-tomorrow&amp;var-severity=critical">Grafana</a>` + "\n"
	if !strings.Contains(msg, want) {
		t.Fatalf("exact Links block missing.\nwant block:\n%s\nfull message:\n%s", want, msg)
	}
}

// TestGrafanaLinkClickableInWirePayload encodes the full integration: the
// alert is rendered, JSON-marshalled, sent to a fake Telegram server, and
// we assert that the captured `text` field contains a real `<a href>`
// anchor (i.e. the bug-report case "Grafana as plain text" is impossible).
func TestGrafanaLinkClickableInWirePayload(t *testing.T) {
	var captured atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Store(body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s, err := NewTelegramSink(TelegramConfig{
		Enabled: true, BotToken: "t", ChatID: "1", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	raw, _ := captured.Load().([]byte)
	var body struct {
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if body.ParseMode != "HTML" {
		t.Fatalf("parse_mode: got %q want HTML", body.ParseMode)
	}
	const wantGrafana = `<a href="http://grafana.public/d/uid123/?from=1&amp;to=2&amp;var-category=Weather&amp;var-market=rain-tomorrow&amp;var-severity=critical">Grafana</a>`
	if !strings.Contains(body.Text, wantGrafana) {
		t.Fatalf("wire payload missing clickable Grafana anchor.\nwant: %s\ngot text:\n%s", wantGrafana, body.Text)
	}
	const wantPolymarket = `<a href="https://polymarket.com/event/rain-tomorrow">Polymarket market</a>`
	if !strings.Contains(body.Text, wantPolymarket) {
		t.Fatalf("wire payload missing clickable Polymarket anchor.\nwant: %s\ngot text:\n%s", wantPolymarket, body.Text)
	}
}

// TestLabelsNeverAppearAsPlainText is the regression guard for the bug
// report: with the URL absent, the label must NOT show up at all (no
// "Grafana" leftover bullet, no orphan bullet). Same for Polymarket-side
// labels.
func TestLabelsNeverAppearAsPlainText(t *testing.T) {
	cases := []struct {
		name string
		mut  func(f *anomaly.Finding)
		// label that must NOT appear as plain text anywhere in the message
		plainLabel string
	}{
		{"no_grafana_url", func(f *anomaly.Finding) { f.GrafanaURL = "" }, "Grafana"},
		{"no_market_url", func(f *anomaly.Finding) { f.MarketURL = "" }, "Polymarket market"},
		{"no_category_url", func(f *anomaly.Finding) { f.CategoryURL = "" }, "Polymarket category"},
		{"no_trader_url", func(f *anomaly.Finding) { f.TraderURL = "" }, "Trader</a>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := sampleTradeFinding()
			c.mut(&f)
			msg := FormatTelegramMessage(f)
			// The label must never appear unwrapped by an anchor closing tag.
			// We allow it to appear *inside* another anchor (other URLs are
			// still present) but not as a bare bulleted line.
			bareBullet := "• " + c.plainLabel
			if strings.Contains(msg, bareBullet) {
				t.Fatalf("plain-text bullet %q must not appear when URL is empty:\n%s", bareBullet, msg)
			}
		})
	}
}

// TestLinksAllOmittedSkipsSectionHeader confirms that the "<b>Links</b>"
// header is itself skipped when no links would render, so we never produce
// a dangling header followed by nothing.
func TestLinksAllOmittedSkipsSectionHeader(t *testing.T) {
	f := sampleTradeFinding()
	f.MarketURL, f.CategoryURL, f.TraderURL, f.GrafanaURL = "", "", "", ""
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "<b>Links</b>") {
		t.Fatalf("Links header must be omitted when no links render:\n%s", msg)
	}
}

// TestLinksOnlyGrafanaPresent guards the edge case where the operator has
// disabled Polymarket public URLs (PolymarketBase=="") but still has
// Grafana wired. Section header renders, Grafana is the only bullet.
func TestLinksOnlyGrafanaPresent(t *testing.T) {
	f := sampleTradeFinding()
	f.MarketURL, f.CategoryURL, f.TraderURL = "", "", ""
	msg := FormatTelegramMessage(f)
	const want = "\n<b>Links</b>\n" +
		`• <a href="http://grafana.public/d/uid123/?from=1&amp;to=2&amp;var-category=Weather&amp;var-market=rain-tomorrow&amp;var-severity=critical">Grafana</a>` + "\n"
	if !strings.Contains(msg, want) {
		t.Fatalf("expected Grafana-only Links block:\n%s\nfull:\n%s", want, msg)
	}
}

// TestSpecialCharsInHrefAreEscaped pins the contract for query strings
// that carry characters Telegram's HTML parser is sensitive to. '&' is
// the common one (every Grafana URL has multiple); '<' is theoretical
// but cheap to cover.
func TestSpecialCharsInHrefAreEscaped(t *testing.T) {
	got := renderLink("Grafana", `https://g.test/d/u/?a=1&b=<x>&c="y"`)
	const want = `<a href="https://g.test/d/u/?a=1&amp;b=&lt;x&gt;&amp;c=&#34;y&#34;">Grafana</a>`
	if got != want {
		t.Fatalf("href escape:\n got: %q\nwant: %q", got, want)
	}
}
