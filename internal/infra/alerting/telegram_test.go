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
			MedianUSD: 9.70, MeanUSD: 12.10, P95USD: 60, P99USD: 250, SampleN: 1240,
			Span:      30*24*time.Hour + 6*time.Hour,
			WindowMax: 365 * 24 * time.Hour,
		},
		TraderBaseline: &anomaly.BaselineRef{
			Scope:     "trader=0xabc",
			MedianUSD: 240, MeanUSD: 480, P95USD: 3_004, P99USD: 9_723, SampleN: 80,
			Span: 88 * 24 * time.Hour,
		},
		Category:            &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather & Climate"},
		GrossPayoutIfWinUSD: 2_400_000,
		ProfitIfWinUSD:      2_280_000,
		MarketP95Ratio:      2_000,
		MarketP99Ratio:      480,
		TraderP95Ratio:      39.95,
		TraderP99Ratio:      12.34,
		PayoffGatePassed:    true,
		TailGatePassed:      true,
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
	// v5 header: <b>SEV: profit $PROFIT · $NOTIONAL · HOT · TITLE</b>
	for _, want := range []string{"<b>", "CRITICAL", "profit $2,280,000", "$120,000", "HOT", "Will it rain &lt;tomorrow&gt;?", "</b>"} {
		if !strings.Contains(first, want) {
			t.Errorf("header missing %q in:\n%s", want, first)
		}
	}
}

func TestTradeAnomalyMessageHasAllRequiredSections(t *testing.T) {
	msg := FormatTelegramMessage(sampleTradeFinding())
	for _, want := range []string{
		"<b>Why</b>",
		"payoff if win: profit <b>$2,280,000</b> (gross $2,400,000)",
		"market tail: notional <b>$120,000</b>",
		"<b>2000x p95</b> ($60.00)",
		"<b>480x p99</b> ($250.00)",
		"trader tail: notional <b>$120,000</b>",
		"<b>40x p95</b> ($3,004)",
		"<b>12x p99</b> ($9,723)",
		"odds <b>20.0</b>, implied probability <b>5.0%</b>",
		"market baseline: <b>1240</b> trades, median $9.70",
		"p95 $60.00, p99 $250.00",
		"trader history: <b>80</b> trades, median $240.00, p95 $3,004, p99 $9,723",
		"span 30d6h",
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

// --- Defensive rendering (v6) -------------------------------------------

// TestHeaderProfitOmittedWhenZero pins the v6 defensive contract: a
// pre-v6 alert payload (or any Finding whose ProfitIfWinUSD = 0) must
// NOT render "profit $0.00" in the header. The header's profit
// segment is OMITTED entirely. Tracking the production bug where a
// legacy v4 payload bled into the v5/v6 binary and rendered
// "INFO: profit $0.00 · $1,830 · ...".
func TestHeaderProfitOmittedWhenZero(t *testing.T) {
	f := sampleTradeFinding()
	f.ProfitIfWinUSD = 0
	f.GrossPayoutIfWinUSD = 0
	msg := FormatTelegramMessage(f)
	first := strings.SplitN(msg, "\n", 2)[0]
	if strings.Contains(first, "profit $") {
		t.Errorf("profit segment must be omitted when ProfitIfWinUSD=0:\n%s", first)
	}
	if !strings.Contains(first, "$120,000") {
		t.Errorf("notional must still render:\n%s", first)
	}
}

// TestHeaderProfitRendersFromV5Payload pins the canonical real-world
// case the user reported: BUY YES, price 0.053, notional $1830 →
// profit ≈ $1830 × (1/0.053 − 1) ≈ $32,720. With the upstream score
// populating ProfitIfWinUSD correctly, the header must show it.
func TestHeaderProfitRendersFromV5Payload(t *testing.T) {
	f := sampleTradeFinding()
	f.Severity = anomaly.SeverityInfo
	f.Trade.NotionalUSD = 1_830
	f.Trade.Price = 0.053
	f.Trade.Odds = 1.0 / 0.053
	notional := 1_830.0
	odds := 1.0 / 0.053
	f.ProfitIfWinUSD = notional * (odds - 1)
	f.GrossPayoutIfWinUSD = notional * odds
	msg := FormatTelegramMessage(f)
	first := strings.SplitN(msg, "\n", 2)[0]
	// Expect the header to carry the ~$32k figure (allow a wide range
	// to dodge formatting precision).
	if !strings.Contains(first, "profit $32,") {
		t.Errorf("expected 'profit $32,...' in header, got:\n%s", first)
	}
}

// TestBaselineRowRendersP99NA pins the v6 defensive rendering: when
// P99USD = 0 (small sample, NULL in DB, or pre-v6 payload), the cell
// reads "p99 n/a" rather than "p99 $0.00".
func TestBaselineRowRendersP99NA(t *testing.T) {
	f := sampleTradeFinding()
	f.Baseline.P99USD = 0
	f.TraderBaseline.P99USD = 0
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "p99 $0.00") {
		t.Errorf("p99 $0.00 must never render — use n/a instead:\n%s", msg)
	}
	if !strings.Contains(msg, "p99 n/a") {
		t.Errorf("expected 'p99 n/a' in:\n%s", msg)
	}
}

// TestTailGateWording_PerAxis pins TASK 4: when the market baseline
// is ready but the trader baseline is thin, the alert must say
// "trader tail: unenforced (only N trader trades on record)" — NOT
// the misleading "tail gate: unenforced (no baseline was ready)".
func TestTailGateWording_PerAxis(t *testing.T) {
	f := sampleTradeFinding()
	f.LowMarketBaselineConfidence = false
	f.LowTraderBaselineConfidence = true
	f.TraderBaseline.SampleN = 4
	f.TailGatePassed = true // market tail did pass
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "no baseline was ready") {
		t.Errorf("must not say 'no baseline was ready' when market was ready:\n%s", msg)
	}
	if !strings.Contains(msg, "trader tail: unenforced (only 4 trader trades on record)") {
		t.Errorf("expected per-axis trader-thin wording:\n%s", msg)
	}
}

// TestTailGateWording_BothAxesThin pins the inverse: when BOTH
// baselines are unready, both per-axis lines render.
func TestTailGateWording_BothAxesThin(t *testing.T) {
	f := sampleTradeFinding()
	f.LowMarketBaselineConfidence = true
	f.LowTraderBaselineConfidence = true
	f.Baseline.SampleN = 0
	f.TraderBaseline.SampleN = 0
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "market tail: unenforced") {
		t.Errorf("missing market-tail unenforced line:\n%s", msg)
	}
	if !strings.Contains(msg, "trader tail: unenforced") {
		t.Errorf("missing trader-tail unenforced line:\n%s", msg)
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

// TestStableFavoriteRendering pins the new alert kind's surface:
// header carries probability + return %, Why block carries
// stability stats, Risks block is mandatory and explicitly says
// "NOT a guarantee" — surveillance language is non-negotiable for
// this strategy.
func TestStableFavoriteRendering(t *testing.T) {
	f := anomaly.Finding{
		Kind:     anomaly.KindStableFavorite,
		Severity: anomaly.SeverityInfo,
		Reason:   anomaly.ReasonStableFavorite,
		At:       time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
		StableFavorite: &anomaly.StableFavoriteRef{
			MarketID:           "0xmassie",
			OutcomeToken:       "tok-yes",
			Outcome:            "Yes",
			Probability:        0.64,
			RemainingReturnPct: 56.25,
			StabilityWindow:    24 * time.Hour,
			PriceMean:          0.64,
			PriceStddev:        0.012,
			PriceMin:           0.62,
			PriceMax:           0.66,
			PriceFirst:         0.63,
			PriceLast:          0.64,
			PriceSamples:       240,
			Drawdown:           0.061,
			AdverseMove6h:      0.0,
			RecentVolumeUSD:    111_000,
			RecentTradeCount:   240,
			LifecyclePct:       96.4,
			Score:              74,
			Confidence:         0.74,
			CrossMarketStatus:  "unavailable",
		},
		LifecyclePct: 96.4,
		DedupKey:     "stable_favorite:informed-flow-v6:0xmassie:tok-yes:info",
	}
	msg := FormatTelegramMessage(f)
	for _, want := range []string{
		"INFO · stable favorite",
		"64% · +56% return",
		"<b>Why</b>",
		"market is <b>96.4%</b> through lifecycle",
		"favorite price stable: 62–66%",
		"remaining return if correct: <b>+56.2%</b>",
		"no adverse drift in last 6h",
		"liquidity: $111,000 volume, 240 recent trades",
		"confidence: <b>0.74</b>",
		"<b>Risks</b>",
		"NOT a guarantee",
		"cross-market check: <b>unavailable</b>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	// Negative pin: must NEVER claim safety.
	for _, forbidden := range []string{"risk-free", "guaranteed", "safe bet"} {
		if strings.Contains(strings.ToLower(msg), forbidden) {
			t.Errorf("forbidden surveillance language %q in:\n%s", forbidden, msg)
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

// TestAnalystNoteOnTradeAnomaly pins the v9.5 contract: the
// "AI analysis" block (formerly "Analyst note") carries the AI text
// when AnalystNote is non-empty. The Data block is gone.
func TestAnalystNoteOnTradeAnomaly(t *testing.T) {
	f := sampleTradeFinding()
	f.DedupKey = "single:v1:trade-1"
	f.AnalystNote = "This looks like a watchlist candidate. Watch the next debate."
	msg := FormatTelegramMessage(f)

	// Positive: AI analysis block must render.
	for _, want := range []string{
		"<b>AI analysis</b>",
		"• This looks like a watchlist candidate. Watch the next debate.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	// Negative: Data block must NOT render — AI analysis replaces it.
	if strings.Contains(msg, "<b>Data</b>") {
		t.Errorf("Data block must be removed in v7:\n%s", msg)
	}
}

// TestAnalystNoteOmittedWhenEmpty: when AnalystNote is empty AND no
// Blocked Alert overlay is stamped, the block must render NOTHING.
func TestAnalystNoteOmittedWhenEmpty(t *testing.T) {
	f := sampleTradeFinding()
	f.DedupKey = "single:v1:trade-1"
	f.AnalystNote = ""
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "<b>AI analysis</b>") {
		t.Errorf("AI analysis must be elided when empty:\n%s", msg)
	}
}

// TestBlockedAlertRendersBeforeAIAnalysis pins PART 4 of the
// Political-Catalyst Intelligence spec: the Blocked Alert block MUST
// appear ABOVE the AI analysis when the Finding carries one.
func TestBlockedAlertRendersBeforeAIAnalysis(t *testing.T) {
	f := sampleTradeFinding()
	f.DedupKey = "single:v1:trade-1"
	f.AnalystNote = "This looks like a watchlist candidate."
	f.Blocked = &anomaly.BlockedAlert{
		Status:               "blocked until Texas GOP runoff results",
		Reason:               "market waiting for final resolution of primary uncertainty",
		CatalystType:         "runoff",
		ExpectedTiming:       "2026-06-15T12:00:00Z",
		BullishScenario:      "decisive Paxton win reprices YES toward 95-99%",
		BearishScenario:      "weak result or recount sharply weakens confidence",
		InvalidationScenario: "disputed outcome extends volatility",
		Stance:               "accumulation before catalyst is more meaningful than late chasing",
	}
	msg := FormatTelegramMessage(f)
	for _, want := range []string{
		"<b>Blocked Alert</b>",
		"• status: blocked until Texas GOP runoff results",
		"• reason: market waiting for final resolution of primary uncertainty",
		"• catalyst type: runoff",
		"• expected timing: 2026-06-15T12:00:00Z",
		"• bullish scenario: decisive Paxton win reprices YES toward 95-99%",
		"• bearish scenario: weak result or recount sharply weakens confidence",
		"• invalidation scenario: disputed outcome extends volatility",
		"• stance: accumulation before catalyst is more meaningful than late chasing",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	// Order: Blocked Alert header must precede AI analysis header.
	iBlocked := strings.Index(msg, "<b>Blocked Alert</b>")
	iAI := strings.Index(msg, "<b>AI analysis</b>")
	if iBlocked < 0 || iAI < 0 {
		t.Fatalf("both blocks must render; iBlocked=%d iAI=%d", iBlocked, iAI)
	}
	if iBlocked >= iAI {
		t.Errorf("Blocked Alert must render BEFORE AI analysis: %d vs %d\n%s", iBlocked, iAI, msg)
	}
}

// TestBlockedAlertEscapesHTML pins the safety contract: Polymarket /
// AI-authored fields are DATA, not markup. The formatter MUST
// HTML-escape every Blocked Alert field so an annotation containing
// `<script>` cannot break Telegram parse mode.
func TestBlockedAlertEscapesHTML(t *testing.T) {
	f := sampleTradeFinding()
	f.DedupKey = "single:v1:trade-1"
	f.AnalystNote = "ok"
	f.Blocked = &anomaly.BlockedAlert{
		Status: "<script>alert(1)</script>",
		Reason: "rogue & special <chars>",
	}
	msg := FormatTelegramMessage(f)
	if strings.Contains(msg, "<script>alert(1)</script>") {
		t.Errorf("unescaped HTML in Blocked Alert:\n%s", msg)
	}
	if !strings.Contains(msg, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected HTML-escaped status:\n%s", msg)
	}
	if !strings.Contains(msg, "rogue &amp; special &lt;chars&gt;") {
		t.Errorf("expected HTML-escaped reason:\n%s", msg)
	}
}

// TestAnalystNoteEscapesHTML protects against a model output that
// contains stray < or > which would break Telegram's HTML parser.
func TestAnalystNoteEscapesHTML(t *testing.T) {
	f := sampleTradeFinding()
	f.AnalystNote = "Watch <emerging> & <breaking> news flow."
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "Watch &lt;emerging&gt; &amp; &lt;breaking&gt; news flow.") {
		t.Errorf("AnalystNote not HTML-escaped:\n%s", msg)
	}
}

// TestOwnershipFindingRendersDistinctHeader pins the Strategy-E
// renderer: an ownership_concentration alert produces a header that
// explicitly carries "ownership concentration · X.X%" and a Why block
// that surfaces the trade-flow approximation caveat.
func TestOwnershipFindingRendersDistinctHeader(t *testing.T) {
	f := anomaly.Finding{
		Kind:     anomaly.KindOwnership,
		Severity: anomaly.SeverityWarning,
		Reason:   anomaly.ReasonOwnership,
		At:       time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC),
		Trade: &anomaly.TradeRef{
			Market:   "0xabc",
			Question: "Will it rain tomorrow?",
		},
		Ownership: &anomaly.OwnershipRef{
			Wallet:            "0xwhale",
			MarketID:          "0xabc",
			OutcomeToken:      "12345",
			Outcome:           "Yes",
			SharePct:          17.4,
			WalletNetShares:   17400,
			MarketTotalShares: 100000,
			NotionalUSD:       42000,
			Approximate:       true,
		},
		Reasons:  []string{"MARKET_OWNERSHIP_CONCENTRATION"},
		DedupKey: "ownership:v4:0xwhale:1:tok:warning",
	}
	msg := FormatTelegramMessage(f)
	for _, want := range []string{
		"<b>WARNING: ownership concentration · 17.4%",
		"• wallet owns <b>17.4%</b> of recorded BUY-side flow on this outcome",
		"• position value estimate: <b>$42,000</b>",
		"• outcome: <b>Yes</b>",
		"<i>trade-flow approximation</i>",
		"• reason: <code>MARKET_OWNERSHIP_CONCENTRATION</code>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in ownership message:\n%s", want, msg)
		}
	}
}

// TestNewWalletContextRendersInAccumulationWhy pins the Strategy-B
// renderer hook: an accumulation Finding carrying a NewWalletRef
// surfaces the "new wallet: first seen X ago" line in the Why block.
func TestNewWalletContextRendersInAccumulationWhy(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	f := anomaly.Finding{
		Kind:     anomaly.KindAccumulation,
		Severity: anomaly.SeverityInfo,
		Reason:   anomaly.ReasonAccumulation,
		At:       now,
		Accumulation: &anomaly.AccumulationRef{
			Wallet:           "0xfresh",
			MarketID:         "0xabc",
			OutcomeToken:     "12345",
			Outcome:          "Yes",
			Side:             "BUY",
			TradeCount:       7,
			TotalNotionalUSD: 50_000,
			Window:           "recent",
		},
		NewWallet: &anomaly.NewWalletRef{
			FirstSeenAt:   now.Add(-12 * time.Hour),
			AgeAtTrade:    12 * time.Hour,
			HistoryTrades: 7,
			IsNew:         true,
		},
	}
	msg := FormatTelegramMessage(f)
	if !strings.Contains(msg, "• <b>new wallet</b>: first seen 12h0m ago, 7 stored trades") {
		t.Errorf("missing new-wallet context line in accumulation Why:\n%s", msg)
	}
	if !strings.Contains(msg, "• window: <b>recent</b>") {
		t.Errorf("missing window line in accumulation Why:\n%s", msg)
	}
}
