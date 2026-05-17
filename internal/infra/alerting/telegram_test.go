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
	s, err := NewTelegramSink(TelegramConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleTradeFinding()); err != nil {
		t.Fatalf("disabled sink returned %v", err)
	}
}

func TestTelegramEnabledRequiresTokenAndChat(t *testing.T) {
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true}); err == nil {
		t.Fatal("expected validation error when enabled w/o creds")
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

	s, err := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "fake", ChatID: "1", BaseURL: srv.URL})
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
	if body["chat_id"] != "1" {
		t.Errorf("chat_id: %v", body["chat_id"])
	}
}

func TestTelegramReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad chat"}`))
	}))
	defer srv.Close()
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", ChatID: "1", BaseURL: srv.URL})
	if err := s.Notify(context.Background(), sampleTradeFinding()); err == nil {
		t.Fatal("expected error for 400")
	}
}
