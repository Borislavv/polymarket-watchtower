package telegram

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
)

func TestBotRequiresToken(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when bot token empty")
	}
	if _, err := New(Config{BotToken: "  "}); err == nil {
		t.Fatal("expected error when bot token blank")
	}
	if _, err := New(Config{BotToken: "real"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestBotSendHTMLParseMode(t *testing.T) {
	var captured atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Store(body)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()

	bot, err := New(Config{BotToken: "fake", BaseURL: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := bot.SendHTML(context.Background(), "1", "<b>hi</b>")
	if err != nil {
		t.Fatalf("SendHTML: %v", err)
	}
	if res.MessageID != 42 {
		t.Fatalf("message id: got %d want 42", res.MessageID)
	}

	raw, _ := captured.Load().([]byte)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if body["parse_mode"] != "HTML" {
		t.Errorf("parse_mode: got %v want HTML", body["parse_mode"])
	}
	if got, _ := body["chat_id"].(float64); int64(got) != 1 {
		t.Errorf("numeric chat id must be int, got %v (%T)", body["chat_id"], body["chat_id"])
	}
	if body["disable_web_page_preview"] != true {
		t.Errorf("disable_web_page_preview missing: %v", body)
	}
}

func TestBotAcceptsChannelUsername(t *testing.T) {
	var captured atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Store(body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot, _ := New(Config{BotToken: "t", BaseURL: srv.URL})
	if _, err := bot.SendHTML(context.Background(), "@watchtower-alerts", "hi"); err != nil {
		t.Fatalf("SendHTML: %v", err)
	}
	raw, _ := captured.Load().([]byte)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["chat_id"] != "@watchtower-alerts" {
		t.Errorf("non-numeric chat id must be forwarded as string, got %v (%T)", body["chat_id"], body["chat_id"])
	}
}

func TestBotReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad chat"}`))
	}))
	defer srv.Close()

	bot, _ := New(Config{BotToken: "t", BaseURL: srv.URL})
	_, err := bot.SendHTML(context.Background(), "1", "x")
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !strings.Contains(err.Error(), "bad chat") {
		t.Errorf("error must include server snippet, got: %v", err)
	}
}

func TestBotSendsExactlyOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot, _ := New(Config{BotToken: "t", BaseURL: srv.URL})
	if _, err := bot.SendHTML(context.Background(), "42", "x"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 send, got %d", got)
	}
}

func TestBotRequiresChatID(t *testing.T) {
	bot, _ := New(Config{BotToken: "t", BaseURL: "http://example.invalid"})
	if _, err := bot.SendHTML(context.Background(), "", "x"); err == nil {
		t.Fatal("expected error on empty chat id")
	}
}

// --- EditMessageText -----------------------------------------------------

// TestEditMessageTextHappyPath pins the canonical edit path: a 200
// from /editMessageText returns nil. We also verify the request
// body carries the new text + parse_mode=HTML.
func TestEditMessageTextHappyPath(t *testing.T) {
	var got struct {
		ChatID    json.Number `json:"chat_id"`
		MessageID int64       `json:"message_id"`
		Text      string      `json:"text"`
		ParseMode string      `json:"parse_mode"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()
	bot, _ := New(Config{BotToken: "t", BaseURL: srv.URL})
	if err := bot.EditMessageText(context.Background(), "42", 42, "new body"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got.MessageID != 42 {
		t.Errorf("message_id: got %d want 42", got.MessageID)
	}
	if got.Text != "new body" {
		t.Errorf("text: got %q", got.Text)
	}
	if got.ParseMode != "HTML" {
		t.Errorf("parse_mode: got %q want HTML", got.ParseMode)
	}
}

// TestEditMessageText400MapsToErrUnsupported pins the contract: a
// 400 (message too old / not modified / parse error) is wrapped in
// ErrEditUnsupported so the caller takes the follow-up-reply path
// instead of retrying.
func TestEditMessageText400MapsToErrUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ok":false,"description":"message can't be edited"}`))
	}))
	defer srv.Close()
	bot, _ := New(Config{BotToken: "t", BaseURL: srv.URL})
	err := bot.EditMessageText(context.Background(), "42", 42, "x")
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !errorsIs(err, ErrEditUnsupported) {
		t.Errorf("error must wrap ErrEditUnsupported, got: %v", err)
	}
}

// TestEditMessageText5xxIsRetryable pins that a 500 returns a plain
// error (not ErrEditUnsupported) so the caller may retry.
func TestEditMessageText5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer srv.Close()
	bot, _ := New(Config{BotToken: "t", BaseURL: srv.URL})
	err := bot.EditMessageText(context.Background(), "42", 42, "x")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errorsIs(err, ErrEditUnsupported) {
		t.Errorf("5xx must NOT map to ErrEditUnsupported: %v", err)
	}
}

func TestEditMessageTextRequiresChatAndMessageID(t *testing.T) {
	bot, _ := New(Config{BotToken: "t", BaseURL: "http://example.invalid"})
	if err := bot.EditMessageText(context.Background(), "", 1, "x"); err == nil {
		t.Error("expected error on empty chat id")
	}
	if err := bot.EditMessageText(context.Background(), "42", 0, "x"); err == nil {
		t.Error("expected error on zero message id")
	}
}

// errorsIs is a tiny wrapper to keep the rest of the file free of
// the errors import (other tests don't need it).
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrap interface{ Unwrap() error }
		u, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
