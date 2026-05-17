package alerting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
			MedianUSD: 9.70, MeanUSD: 12.10, P95USD: 60, SampleN: 1240, WindowAgo: 7 * 24 * time.Hour,
		},
		Category:       &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather & Climate"},
		Multiplier:     12_371,
		AbsoluteTier:   anomaly.SeverityCritical,
		MultiplierTier: anomaly.SeverityCritical,
		MarketURL:      "https://polymarket.com/event/rain-tomorrow",
		GrafanaURL:     "http://grafana.local/d/uid123/?from=1&to=2&var-category=Weather&var-market=rain-tomorrow&var-severity=critical",
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
	s, err := NewTelegramSink(TelegramConfig{Enabled: false}, nil)
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("disabled sink returned %v", err)
	}
}

func TestTelegramEnabledRequiresTokenAndSubscribers(t *testing.T) {
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true}, nil); err == nil {
		t.Fatal("expected error w/o token")
	}
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t"}, nil); err == nil {
		t.Fatal("expected error w/o subscribers")
	}
}

func TestTradeAnomalyHeaderFormat(t *testing.T) {
	msg := FormatTelegramMessage(sampleTradeFinding())
	first := strings.SplitN(msg, "\n", 2)[0]
	// Header: <b>SEV: xMUL · $NOTIONAL · TITLE</b>
	for _, want := range []string{"<b>", "CRITICAL", "x12371", "$120,000", "Will it rain &lt;tomorrow&gt;?", "</b>"} {
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
		"tiers: absolute=<code>critical</code> multiplier=<code>critical</code>",
		"<b>Trade</b>",
		"outcome: <b>Yes</b> (BUY)",
		"size: $120,000",
		"trader: <code>0xabc1234567890def1234567890abcdef12345678</code>",
		"category: Weather &amp; Climate", // & must be escaped
		"<b>Links</b>",
		`<a href="https://polymarket.com/event/rain-tomorrow">Polymarket</a>`,
		`<a href="http://grafana.local/d/uid123/?from=1&amp;to=2&amp;var-category=Weather&amp;var-market=rain-tomorrow&amp;var-severity=critical">Grafana</a>`,
	} {
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

func TestTelegramHTMLParseMode(t *testing.T) {
	var received atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received.Store(body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	subs := NewSubscribers("1")
	s, err := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "fake", BaseURL: srv.URL}, subs)
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

func TestTelegramReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", BaseURL: srv.URL}, NewSubscribers("1"))
	if err := s.Notify(context.Background(), sampleTradeFinding()); err == nil {
		t.Fatal("expected error for 400")
	}
}

func TestBroadcastSendsToEveryChat(t *testing.T) {
	var seen sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ChatID int64 `json:"chat_id"`
		}
		_ = json.Unmarshal(body, &p)
		seen.Store(p.ChatID, struct{}{})
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	subs := NewSubscribers("10", "20", "30")
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", BaseURL: srv.URL}, subs)
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	for _, id := range []int64{10, 20, 30} {
		if _, ok := seen.Load(id); !ok {
			t.Errorf("chat %d did not receive broadcast", id)
		}
	}
}

func TestBroadcastContinuesAfterPerChatError(t *testing.T) {
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			ChatID int64 `json:"chat_id"`
		}
		_ = json.Unmarshal(body, &p)
		if p.ChatID == 20 {
			w.WriteHeader(400)
			return
		}
		sent.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	subs := NewSubscribers("10", "20", "30")
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", BaseURL: srv.URL}, subs)
	if err := s.Notify(context.Background(), sampleTradeFinding()); err == nil {
		t.Fatal("expected surfacing error")
	}
	if got := sent.Load(); got != 2 {
		t.Fatalf("expected 2 successful sends, got %d", got)
	}
}

func TestSubscribersAddDedupesAndSnapshots(t *testing.T) {
	s := NewSubscribers("1", "", "abc", "2")
	if s.Size() != 2 {
		t.Fatalf("seed size: %d", s.Size())
	}
	if !s.Add(3) || s.Add(3) {
		t.Fatal("Add dedupe broken")
	}
	if s.Size() != 3 {
		t.Fatalf("size: %d", s.Size())
	}
}

func TestSubscribersConcurrentAddRaceSafe(t *testing.T) {
	s := NewSubscribers()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				s.Add(int64(seed*1000 + j))
			}
		}(i)
	}
	wg.Wait()
	if s.Size() != 4000 {
		t.Fatalf("expected 4000 unique ids, got %d", s.Size())
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
