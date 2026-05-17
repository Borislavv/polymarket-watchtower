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
		Reason:   "multiplier+absolute_tier",
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Trade: &anomaly.TradeRef{
			ID:          "trade-1",
			Wallet:      "0xabc1234567890def1234567890abcdef12345678",
			Market:      "0xabc",
			Slug:        "rain-tomorrow",
			Question:    "Will it rain *tomorrow*?",
			Outcome:     "Yes",
			Side:        trade.SideBuy,
			SizeShares:  4_000,
			Price:       0.5,
			NotionalUSD: 2_000_000, // big number, with commas
			At:          time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		},
		Baseline: &anomaly.BaselineRef{
			Scope:     "category=Weather market=rain-tomorrow outcome=Yes",
			MedianUSD: 10, MeanUSD: 12, P95USD: 60, SampleN: 1234, WindowAgo: 7 * 24 * time.Hour,
		},
		Category:     &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather"},
		Multiplier:   200_000,
		AbsoluteTier: 100_000,
		MarketURL:    "https://polymarket.com/event/rain-tomorrow",
		GrafanaURL:   "http://grafana.local/d/uid123/?orgId=1&from=1&to=2&var-category=Weather&var-market=rain-tomorrow",
	}
}

func sampleClusterFinding() anomaly.Finding {
	return anomaly.Finding{
		Kind:     anomaly.KindCategoryWatch,
		Severity: anomaly.SeverityHard,
		At:       time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Category: &anomaly.CategoryRef{ID: 99, Slug: "weather", Label: "Weather"},
		Cluster: &anomaly.ClusterStats{
			Window:          time.Hour,
			AnomalousTrades: 7,
			UniqueWallets:   5,
			TotalUSD:        125_000,
			Sample: []anomaly.TradeRef{
				{Question: "Will it rain?", NotionalUSD: 50_000, Outcome: "Yes", Wallet: "0xabc1234567890def1234567890abcdef12345678"},
				{Question: "Snow on Friday?", NotionalUSD: 40_000, Outcome: "No", Wallet: "0xfeed4567890abc1234567890abcdef12345678ab"},
			},
		},
		GrafanaURL: "http://grafana.local/d/uid123/?orgId=1&var-category=Weather",
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
		t.Fatal("expected validation error when enabled w/o token")
	}
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t"}, nil); err == nil {
		t.Fatal("expected validation error when enabled w/o subscribers")
	}
}

func TestNoSubscribersIsSilentNotError(t *testing.T) {
	// A live bot may have no subscribers yet — alerts should silently no-op
	// rather than spam the log with errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not POST when there are no subscribers")
	}))
	defer srv.Close()
	subs := NewSubscribers() // empty
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", BaseURL: srv.URL}, subs)
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestTradeAnomalyMessageHasAllRequiredFields(t *testing.T) {
	msg := FormatTelegramMessage(sampleTradeFinding())
	for _, want := range []string{
		"CRITICAL",
		`multiplier+absolute\_tier`,    // why selected (markdown-escaped)
		"Will it rain \\*tomorrow\\*?", // market question, escaped
		"outcome: `Yes`",
		"side: `BUY`",
		"$2,000,000", // formatted notional
		"category: *Weather*",
		"baseline: median *$10",
		"N=`1234`",
		"multiplier: *x200000*",
		"absolute tier crossed: *$100,000*",
		"polymarket.com/event/rain-tomorrow",
		"grafana.local",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestCategoryWatchMessageHasAllRequiredFields(t *testing.T) {
	msg := FormatTelegramMessage(sampleClusterFinding())
	for _, want := range []string{
		"HARD",
		"CATEGORY WATCH REQUIRED",
		"category: *Weather*",
		"7 anomalous trades",
		"5 unique wallets",
		"$125,000",
		"1h",
		"Will it rain?",
		"Snow on Friday?",
		"$50,000",
		"$40,000",
		"grafana.local",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestTelegramSendsFormattedMessage(t *testing.T) {
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
	if raw == nil {
		t.Fatal("server received no request")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	// chat_id is numeric in the JSON payload.
	if got, _ := body["chat_id"].(float64); int64(got) != 1 {
		t.Errorf("chat_id: got %v want 1", body["chat_id"])
	}
}

func TestTelegramReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad chat"}`))
	}))
	defer srv.Close()
	subs := NewSubscribers("1")
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", BaseURL: srv.URL}, subs)
	if err := s.Notify(context.Background(), sampleTradeFinding()); err == nil {
		t.Fatal("expected error for 400")
	}
}

func TestBroadcastSendsToEveryChat(t *testing.T) {
	var seen sync.Map // chat id -> struct{}
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
			t.Errorf("chat %d did not receive the broadcast", id)
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
	err := s.Notify(context.Background(), sampleTradeFinding())
	if err == nil {
		t.Fatal("expected the failing chat to surface as error")
	}
	if got := sent.Load(); got != 2 {
		t.Errorf("expected 2 successful sends, got %d", got)
	}
}

func TestSubscribersAddDedupesAndSnapshots(t *testing.T) {
	s := NewSubscribers("1", "", "abc", "2") // empty + bad input skipped
	if got := s.Size(); got != 2 {
		t.Fatalf("seed size: %d", got)
	}
	if !s.Add(3) {
		t.Fatal("first add should be true")
	}
	if s.Add(3) {
		t.Fatal("duplicate add should be false")
	}
	if got := s.Size(); got != 3 {
		t.Fatalf("size after add: %d", got)
	}
	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len: %d", len(snap))
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
	if got := s.Size(); got != 4000 {
		t.Fatalf("expected 4000 unique ids, got %d", got)
	}
}
