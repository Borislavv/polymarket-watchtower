package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
)

func newClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL:  baseURL,
		Timeout:  2 * time.Second,
		MaxRetry: 3,
		Limiter:  ratelimit.Noop{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGetJSONOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"x": 7})
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	var out struct{ X int }
	if err := c.GetJSON(context.Background(), "/", nil, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.X != 7 {
		t.Fatalf("decoded: %+v", out)
	}
}

func TestGetJSONRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(429)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"x": 1})
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	var out struct{ X int }
	if err := c.GetJSON(context.Background(), "/", nil, &out); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestGetJSONRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	var out struct{}
	if err := c.GetJSON(context.Background(), "/", nil, &out); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestGetJSONReturns404AsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	var out struct{}
	err := c.GetJSON(context.Background(), "/", nil, &out)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGetJSONNonRetryable4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	var out struct{}
	err := c.GetJSON(context.Background(), "/x", nil, &out)
	if err == nil {
		t.Fatal("expected error for 400")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != 400 || apiErr.Retryable() {
		t.Fatalf("status=%d retryable=%v body=%q", apiErr.Status, apiErr.Retryable(), apiErr.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("4xx must not retry, got %d calls", calls.Load())
	}
}

func TestGetJSONMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	var out struct{ X int }
	if err := c.GetJSON(context.Background(), "/", nil, &out); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGetJSONContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until client cancels
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	var out struct{}
	err := c.GetJSON(ctx, "/", nil, &out)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestObserveHookCalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var seen atomic.Int32
	c, err := New(Config{
		BaseURL: srv.URL,
		Limiter: ratelimit.Noop{},
		Observe: func(endpoint string, status int, dur time.Duration) {
			seen.Add(1)
			if status != 200 {
				t.Errorf("status: %d", status)
			}
			if endpoint != "/foo" {
				t.Errorf("endpoint: %s", endpoint)
			}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out struct{}
	_ = c.GetJSON(context.Background(), "/foo", nil, &out)
	if seen.Load() != 1 {
		t.Fatalf("observe called %d times, want 1", seen.Load())
	}
}
