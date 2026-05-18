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
		Category:            &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather & Climate"},
		MarketMultiplier:    12_371,
		EffectiveMultiplier: 12_371,
		MultiplierAxis:      "market",
		AbsoluteTier:        anomaly.SeverityCritical,
		MultiplierTier:      anomaly.SeverityCritical,
		MarketURL:           "https://polymarket.com/event/rain-tomorrow",
		CategoryURL:         "https://polymarket.com/predictions/weather",
		TraderURL:           "https://polymarket.com/profile/0xabc1234567890def1234567890abcdef12345678",
		GrafanaURL:          "http://grafana.public/d/uid123/?from=1&to=2&var-category=Weather&var-market=rain-tomorrow&var-severity=critical",
		LifecyclePct:        93.5,
		Hot:                 true,
		InCluster:           true,
		ClusterPeerCount:    4,
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
		"<b>x12371</b> above market baseline median ($9.70)",
		"odds <b>20.0</b>, implied probability <b>5.0%</b>",
		"baseline: <b>1240</b> trades, median $9.70",
		"span 30d6h",
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

// TestHumanDurationSubMinuteIsHTMLSafe pins the regression fixed for the
// Bot API "Unsupported start tag \"1m\"" error: humanDuration must never
// emit a literal `<` that lands in HTML-parse-mode output. The previous
// implementation returned the raw string "<1m" for sub-minute spans, which
// Telegram parsed as the start of an unknown tag named "1m" and rejected
// the entire message with HTTP 400.
func TestHumanDurationSubMinuteIsHTMLSafe(t *testing.T) {
	cases := []time.Duration{0, -1, time.Second, 30 * time.Second, time.Minute - 1}
	for _, d := range cases {
		out := humanDuration(d)
		if strings.Contains(out, "<") && !strings.Contains(out, "&lt;") {
			t.Errorf("humanDuration(%v) emitted raw '<': %q", d, out)
		}
		// Cheap structural check: anything containing `<` MUST also contain `&lt;`
		// (we only ever emit the entity, never a real tag).
		if strings.Contains(out, "<") {
			if !strings.HasPrefix(out, "&lt;") {
				t.Errorf("humanDuration(%v) must start with '&lt;' when sub-minute, got %q", d, out)
			}
		}
	}
}

// TestFormatTelegramMessageNeverEmitsRawLT walks the formatted output for
// a Finding whose baseline span is sub-minute (the trigger condition in
// production that broke v4 alerts) and asserts the message contains no
// literal `<1m` token. The Telegram HTML parser is strict — any unknown
// `<…>` short-circuits the whole message.
func TestFormatTelegramMessageNeverEmitsRawLT(t *testing.T) {
	f := sampleTradeFinding()
	f.Baseline.Span = 30 * time.Second // sub-minute span
	if got := FormatTelegramMessage(f); strings.Contains(got, "<1m") {
		t.Fatalf("formatted message contains raw '<1m' (would break Telegram HTML parser):\n%s", got)
	}
	// Confirm the visible-text form is preserved as an HTML entity.
	if got := FormatTelegramMessage(f); !strings.Contains(got, "&lt;1m") {
		t.Errorf("expected '&lt;1m' entity in sub-minute baseline span output:\n%s", got)
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

// TestSanitizeLinkURLRejectsNonReachable pins the load-bearing defence
// against the bug where the docker-compose default
// GRAFANA_BASE_URL=http://localhost:3000 leaked into production alerts:
// Telegram refused to make localhost links clickable and operators saw a
// "Grafana" anchor that did nothing on mobile.
//
// All forms an operator might paste without thinking — localhost,
// loopback IPv4, loopback IPv6, unspecified, link-local — must be elided
// at the formatter so the Links section never carries a dead bullet.
func TestSanitizeLinkURLRejectsNonReachable(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:3000",
		"http://LOCALHOST/d/uid",
		"https://localhost",
		"http://127.0.0.1:3000",
		"http://127.0.0.1/d/uid",
		"http://[::1]:3000",
		"http://0.0.0.0:3000",
		"http://169.254.169.254/latest", // EC2 link-local
	} {
		if got := sanitizeLinkURL(raw); got != "" {
			t.Errorf("sanitizeLinkURL(%q) = %q, want empty (non-reachable host)", raw, got)
		}
	}
}

// TestSanitizeLinkURLRejectsUnsafeSchemes blocks javascript:, data:, file:
// and other schemes that would either be inert in Telegram or, worse,
// trigger client-side behaviour. Bare strings and obviously-broken URLs
// also drop to empty so they cannot accidentally render as a link.
func TestSanitizeLinkURLRejectsUnsafeSchemes(t *testing.T) {
	for _, raw := range []string{
		"",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"ftp://files.example.com/grafana",
		"grafana.public/d/uid", // no scheme
		"://broken",
	} {
		if got := sanitizeLinkURL(raw); got != "" {
			t.Errorf("sanitizeLinkURL(%q) = %q, want empty (unsafe / unparseable)", raw, got)
		}
	}
}

// TestSanitizeLinkURLAcceptsPublic confirms the happy path: public http
// and https URLs survive unchanged, including the query parameters
// Grafana deep-links lean on (from/to/var-*). The sanitizer must NOT
// strip or re-encode anything — only validate.
func TestSanitizeLinkURLAcceptsPublic(t *testing.T) {
	for _, raw := range []string{
		"https://polymarket.com/event/rain-tomorrow",
		"https://grafana.example.com/d/uid123/?from=1&to=2&var-category=Weather&var-severity=critical",
		"http://grafana.public/d/uid",
		"http://192.0.2.10/d/uid", // TEST-NET-1, public-routable per IANA
	} {
		if got := sanitizeLinkURL(raw); got != raw {
			t.Errorf("sanitizeLinkURL(%q) = %q, want unchanged", raw, got)
		}
	}
}

// TestLinksElideLocalhostGrafana is the end-to-end regression: an
// operator running with the docker-compose default
// GRAFANA_BASE_URL=http://localhost:3000 must not see a Grafana bullet
// in the rendered Telegram body at all (no anchor, no plain-text label,
// no orphan bullet). The Links section should still render — the other
// three URLs are public — but Grafana is invisible.
func TestLinksElideLocalhostGrafana(t *testing.T) {
	f := sampleTradeFinding()
	f.GrafanaURL = "http://localhost:3000/d/uid123/?from=1&to=2&var-severity=critical"
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "Grafana") {
		t.Fatalf("localhost Grafana URL must not produce any Grafana entry in body:\n%s", msg)
	}
	// Other links survive.
	for _, want := range []string{"Polymarket market", "Polymarket category", "Trader"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing public link %q in:\n%s", want, msg)
		}
	}
}

// TestLinksElideAllLocalhostSkipsSection covers the worst-case operator
// misconfig: every URL is a localhost / loopback variant. The Links
// section should be omitted entirely rather than emitting an empty
// header.
func TestLinksElideAllLocalhostSkipsSection(t *testing.T) {
	f := sampleTradeFinding()
	f.MarketURL = "http://localhost/event/x"
	f.CategoryURL = "http://127.0.0.1/predictions/x"
	f.TraderURL = "http://[::1]/profile/0xabc"
	f.GrafanaURL = "http://localhost:3000/d/uid"
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "<b>Links</b>") {
		t.Fatalf("Links section must be skipped when every URL is non-reachable:\n%s", msg)
	}
}

// TestDataBlockOnTradeAnomaly pins the trailing Data block on a
// single-trade Finding. The block carries the dedup_key (primary join
// key for log / Grafana correlation) plus the market_id from the
// firing trade. Values are wrapped in <code> for copy-friendliness.
func TestDataBlockOnTradeAnomaly(t *testing.T) {
	f := sampleTradeFinding()
	f.DedupKey = "single:v1:trade-1"
	msg := FormatTelegramMessage(f)
	for _, want := range []string{
		"<b>Data</b>",
		"• market_id: <code>0xabc</code>",
		"• dedup: <code>single:v1:trade-1</code>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	// Data block must come AFTER Links — operators read top-down: severity,
	// why, trade, links to act, raw identifiers last.
	if li, di := strings.Index(msg, "<b>Links</b>"), strings.Index(msg, "<b>Data</b>"); !(li > 0 && di > li) {
		t.Fatalf("Data must follow Links in layout:\n%s", msg)
	}
}

// TestDataBlockOnAccumulation verifies the accumulation Finding
// surfaces market_id + outcome_token from AccumulationRef. The
// accumulation Finding is the only kind that carries the CLOB token
// pre-computed (the line is by construction single-outcome).
func TestDataBlockOnAccumulation(t *testing.T) {
	f := anomaly.Finding{
		Kind:     anomaly.KindAccumulation,
		Severity: anomaly.SeverityWarning,
		Reason:   anomaly.ReasonAccumulation,
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Accumulation: &anomaly.AccumulationRef{
			Wallet:           "0xabc1234567890def1234567890abcdef12345678",
			MarketID:         "0xmarket",
			OutcomeToken:     "12345678901234567890",
			Outcome:          "Yes",
			Side:             "BUY",
			TradeCount:       7,
			TotalNotionalUSD: 80_000,
			Span:             2 * time.Hour,
		},
		Trade: &anomaly.TradeRef{
			Market:      "0xmarket",
			NotionalUSD: 8_000,
		},
		DedupKey: "accumulation:v1:0xabc:1:t:BUY:1747483200",
	}
	msg := FormatTelegramMessage(f)
	for _, want := range []string{
		"<b>Data</b>",
		"• market_id: <code>0xmarket</code>",
		"• outcome_token: <code>12345678901234567890</code>",
		"• dedup: <code>accumulation:v1:0xabc:1:t:BUY:1747483200</code>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

// TestDataBlockOnCluster confirms cluster findings render the dedup
// key. Cluster findings legitimately have no single market_id (a
// cluster spans the category), so market_id may be missing — but the
// block itself must render so the dedup key is visible.
func TestDataBlockOnCluster(t *testing.T) {
	f := sampleClusterFinding()
	f.DedupKey = "cluster:v1:99:1747483200"
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "<b>Data</b>") {
		t.Fatalf("cluster finding missing Data block:\n%s", msg)
	}
	if !strings.Contains(msg, "• dedup: <code>cluster:v1:99:1747483200</code>") {
		t.Fatalf("cluster dedup not rendered:\n%s", msg)
	}
}

// TestDataBlockOmittedWhenAllEmpty keeps the older trade-only fixtures
// (those that don't set DedupKey) rendering exactly as before — no
// orphan "Data" header, no empty block.
func TestDataBlockOmittedWhenAllEmpty(t *testing.T) {
	f := anomaly.Finding{
		Kind:     anomaly.KindCategoryWatch,
		Severity: anomaly.SeverityHard,
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Category: &anomaly.CategoryRef{ID: 1, Slug: "x", Label: "X"},
		Cluster:  &anomaly.ClusterStats{Window: time.Minute, AnomalousTrades: 1, UniqueWallets: 1, TotalUSD: 1},
	}
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "<b>Data</b>") {
		t.Fatalf("Data block must be skipped when dedup + market + token are all empty:\n%s", msg)
	}
}

// TestDataBlockEscapesValues protects against an alert payload that
// (hypothetically) embedded HTML-unsafe characters in identifier
// fields. dedup_key today is alphanumeric + ':' + '-', but defence in
// depth matters: a future strategy version is free to widen the
// charset and we don't want a stray '<' to break Telegram's HTML
// parser.
func TestDataBlockEscapesValues(t *testing.T) {
	f := sampleTradeFinding()
	f.Trade.Market = "0x<evil>"
	f.DedupKey = `single:v1:a&b<c>`
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "• market_id: <code>0x&lt;evil&gt;</code>") {
		t.Errorf("market_id not HTML-escaped:\n%s", msg)
	}
	if !strings.Contains(msg, "• dedup: <code>single:v1:a&amp;b&lt;c&gt;</code>") {
		t.Errorf("dedup not HTML-escaped:\n%s", msg)
	}
}
