package alerting

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

func TestPollerParsesChatIDsAndAdvancesOffset(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			if got := r.URL.Query().Get("offset"); got != "" {
				t.Errorf("first call must omit offset, got %q", got)
			}
			_, _ = w.Write([]byte(`{
				"ok": true,
				"result": [
					{"update_id": 100, "message":             {"chat": {"id": 111}}},
					{"update_id": 101, "edited_message":      {"chat": {"id": 222}}},
					{"update_id": 102, "channel_post":        {"chat": {"id": 333}}},
					{"update_id": 103, "edited_channel_post": {"chat": {"id": 444}}},
					{"update_id": 104, "my_chat_member":      {"chat": {"id": 555}}}
				]
			}`))
		case 2:
			// Offset must equal lastUpdateID + 1 = 105.
			if got := r.URL.Query().Get("offset"); got != "105" {
				t.Errorf("second call offset: got %q want 105", got)
			}
			_, _ = w.Write([]byte(`{"ok": true, "result": []}`))
		default:
			t.Fatalf("unexpected call %d", calls.Load())
		}
	}))
	defer srv.Close()

	subs := NewSubscribers()
	log := zerolog.Nop()
	p, err := NewPoller(PollerConfig{BotToken: "fake", BaseURL: srv.URL}, subs, &log)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	want := map[int64]bool{111: true, 222: true, 333: true, 444: true, 555: true}
	for _, id := range subs.Snapshot() {
		if !want[id] {
			t.Errorf("unexpected chat id %d", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("missing chat ids: %v", want)
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
}

func TestPollerDedupesAcrossTicks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok": true, "result": [
			{"update_id": 1, "message": {"chat": {"id": 999}}}
		]}`))
	}))
	defer srv.Close()
	subs := NewSubscribers()
	log := zerolog.Nop()
	p, _ := NewPoller(PollerConfig{BotToken: "fake", BaseURL: srv.URL}, subs, &log)
	for i := 0; i < 3; i++ {
		_ = p.Tick(context.Background())
	}
	if got := subs.Size(); got != 1 {
		t.Fatalf("expected 1 unique subscriber, got %d", got)
	}
}

func TestPollerIgnoresUpdatesWithoutChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Update with no recognised payload field — must not crash, must
		// still advance the offset.
		_, _ = w.Write([]byte(`{"ok": true, "result": [
			{"update_id": 42, "poll_answer": {"option_ids": [1]}}
		]}`))
	}))
	defer srv.Close()
	subs := NewSubscribers()
	log := zerolog.Nop()
	p, _ := NewPoller(PollerConfig{BotToken: "fake", BaseURL: srv.URL}, subs, &log)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := subs.Size(); got != 0 {
		t.Fatalf("expected 0 subscribers from unrecognised update, got %d", got)
	}
}

func TestPollerSurfacesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok": false, "description": "Unauthorized"}`))
	}))
	defer srv.Close()
	subs := NewSubscribers()
	log := zerolog.Nop()
	p, _ := NewPoller(PollerConfig{BotToken: "fake", BaseURL: srv.URL}, subs, &log)
	if err := p.Tick(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestPollerSurfacesNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok": false, "description": "Conflict: webhook is set"}`))
	}))
	defer srv.Close()
	subs := NewSubscribers()
	log := zerolog.Nop()
	p, _ := NewPoller(PollerConfig{BotToken: "fake", BaseURL: srv.URL}, subs, &log)
	err := p.Tick(context.Background())
	if err == nil {
		t.Fatal("expected error when ok=false")
	}
}

func TestPollerStopsOnContextCancel(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok": true, "result": []}`))
	}))
	defer srv.Close()
	subs := NewSubscribers()
	log := zerolog.Nop()
	p, _ := NewPoller(PollerConfig{BotToken: "fake", BaseURL: srv.URL, Interval: 50}, subs, &log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	// Smoke: the initial tick may or may not have completed before cancel;
	// just ensure we never loop forever (the test would time out otherwise).
	_ = strconv.Itoa(int(calls.Load()))
}
