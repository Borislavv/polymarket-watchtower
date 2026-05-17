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
)

func sampleFinding() anomaly.Finding {
	return anomaly.Finding{
		At:          time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Scope:       anomaly.ScopeMarket,
		Market:      "0xabc",
		Label:       "Will it rain *tomorrow*?",
		MarketURL:   "https://polymarket.com/event/rain-tomorrow",
		Metric:      anomaly.MetricNotionalRate,
		Severity:    anomaly.SeverityCritical,
		Multiplier:  157.3,
		Recent:      2400,
		Baseline:    15,
		WindowLen:   12 * time.Hour,
		BaselineLen: 7 * 24 * time.Hour,
	}
}

func TestTelegramDisabledIsNoop(t *testing.T) {
	s, err := NewTelegramSink(TelegramConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleFinding()); err != nil {
		t.Fatalf("disabled sink returned %v", err)
	}
}

func TestTelegramEnabledRequiresTokenAndChat(t *testing.T) {
	if _, err := NewTelegramSink(TelegramConfig{Enabled: true}); err == nil {
		t.Fatal("expected validation error when enabled w/o creds")
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

	s, err := NewTelegramSink(TelegramConfig{
		Enabled: true, BotToken: "fake", ChatID: "1", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewTelegramSink: %v", err)
	}
	if err := s.Notify(context.Background(), sampleFinding()); err != nil {
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
	text, _ := body["text"].(string)
	for _, want := range []string{"CRITICAL", "0xabc", "x157", "polymarket.com/event/rain"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}

func TestTelegramReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad chat"}`))
	}))
	defer srv.Close()
	s, _ := NewTelegramSink(TelegramConfig{Enabled: true, BotToken: "t", ChatID: "1", BaseURL: srv.URL})
	if err := s.Notify(context.Background(), sampleFinding()); err == nil {
		t.Fatal("expected error for 400")
	}
}

func TestFormatMessageEscapesMarkdown(t *testing.T) {
	f := sampleFinding()
	f.Label = "_underscore_ *star* `tick`"
	msg := FormatTelegramMessage(f)
	for _, want := range []string{`\_underscore\_`, `\*star\*`, "\\`tick\\`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing escape %q in:\n%s", want, msg)
		}
	}
}
