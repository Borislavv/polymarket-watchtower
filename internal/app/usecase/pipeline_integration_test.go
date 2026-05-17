// Package usecase_test holds end-to-end pipeline tests that wire real
// discover/collect/detect against httptest-backed upstreams. Nothing in here
// touches the public internet.
package usecase_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/aggregate"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/discover"
	anomaly2 "github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	alerting2 "github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
	"github.com/rs/zerolog"
)

type capturingSink struct {
	mu sync.Mutex
	fs []anomaly2.Finding
}

func (s *capturingSink) Name() string { return "capture" }
func (s *capturingSink) Notify(_ context.Context, f anomaly2.Finding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fs = append(s.fs, f)
	return nil
}
func (s *capturingSink) Findings() []anomaly2.Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]anomaly2.Finding, len(s.fs))
	copy(out, s.fs)
	return out
}

func TestPipelineEndToEndAnomalyFiresAndReachesTelegram(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// --- Gamma fake: 1 market with 1 tag ---------------------------------
	gammaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tags":
			if r.URL.Query().Get("offset") == "0" {
				_, _ = w.Write([]byte(`[{"id":"42","slug":"politics","label":"Politics"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case "/events":
			if r.URL.Query().Get("offset") == "0" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"id": "e1", "slug": "us-pres", "active": true,
					"tags":    []map[string]any{{"id": "42", "slug": "politics", "label": "Politics"}},
					"markets": []map[string]any{{"conditionId": "0xa", "slug": "us-pres", "active": true}},
				}})
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case "/markets":
			if r.URL.Query().Get("offset") == "0" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"conditionId": "0xa",
					"slug":        "us-pres",
					"question":    "Who wins?",
					"active":      true,
				}})
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer gammaSrv.Close()

	// --- Data API fake: 60 baseline trades + 1500 recent trades ----------
	type t1 struct {
		ID        string  `json:"id"`
		Market    string  `json:"market"`
		Asset     string  `json:"asset"`
		Side      string  `json:"side"`
		Size      float64 `json:"size"`
		Price     float64 `json:"price"`
		Timestamp int64   `json:"timestamp"`
	}
	var trades []t1
	for i := 0; i < 60; i++ {
		trades = append(trades, t1{
			ID: "b" + strconv.Itoa(i), Market: "0xa", Asset: "1",
			Side: "BUY", Size: 1, Price: 0.5,
			Timestamp: now.Add(-24 * time.Hour).Add(time.Duration(i) * time.Minute).Unix(),
		})
	}
	for i := 0; i < 1500; i++ {
		trades = append(trades, t1{
			ID: "r" + strconv.Itoa(i), Market: "0xa", Asset: "1",
			Side: "BUY", Size: 10, Price: 0.5,
			Timestamp: now.Add(-time.Hour).Add(time.Duration(i*2) * time.Second).Unix(),
		})
	}
	dataCalls := atomic.Int32{}
	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dataCalls.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 500
		}
		if offset >= len(trades) {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		end := offset + limit
		if end > len(trades) {
			end = len(trades)
		}
		_ = json.NewEncoder(w).Encode(trades[offset:end])
	}))
	defer dataSrv.Close()

	// --- Telegram fake -----------------------------------------------------
	telegramCalls := atomic.Int32{}
	var telegramBody atomic.Value
	telegramSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		telegramCalls.Add(1)
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		telegramBody.Store(string(buf[:n]))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegramSrv.Close()

	// --- wire pipeline ----------------------------------------------------
	gh, _ := httpx.New(httpx.Config{BaseURL: gammaSrv.URL, Limiter: ratelimit.Noop{}})
	dh, _ := httpx.New(httpx.Config{BaseURL: dataSrv.URL, Limiter: ratelimit.Noop{}})
	gammaClient := gamma.New(gh)
	dataClient := dataapi.New(dh)

	met := metrics.New()
	reg := aggregate.NewRegistry()
	eng := aggregate.New(aggregate.Config{
		Bucket: time.Minute, Baseline: 26 * time.Hour, Clock: clock,
	})

	log := zerolog.Nop()
	disc := discover.New(discover.Config{
		Interval: time.Hour, ActiveOnly: true, MaxMarkets: 100,
	}, gammaClient, reg, eng, met, &log)
	collectLoop := collect.New(collect.Config{
		Interval: time.Hour, Concurrency: 1, LookbackBoot: 25 * time.Hour, Clock: clock,
	}, dataClient, eng, reg, met, &log)

	tg, err := alerting2.NewTelegramSink(alerting2.TelegramConfig{
		Enabled: true, BotToken: "test", ChatID: "1", BaseURL: telegramSrv.URL,
	})
	if err != nil {
		t.Fatalf("telegram sink: %v", err)
	}
	cap := &capturingSink{}
	fanout := &alerting2.Fanout{Sinks: []alerting2.Sink{cap, tg}, Logger: &log}

	det := detect.New(detect.Config{
		Interval:      time.Hour,
		RecentWindows: []time.Duration{time.Hour},
		Rule:          anomaly2.Rule{Multipliers: []float64{30, 100, 1000}},
		Cooldown:      time.Hour,
		Clock:         clock,
	}, eng, reg, fanout, met, &log)

	ctx := context.Background()

	// --- act --------------------------------------------------------------
	if err := disc.RunOnce(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}
	collectLoop.Tick(ctx)
	det.Tick(ctx)

	// --- assert -----------------------------------------------------------
	if reg.Size() != 1 {
		t.Fatalf("registry size: %d", reg.Size())
	}
	if dataCalls.Load() == 0 {
		t.Fatalf("data API not called")
	}
	findings := cap.Findings()
	if len(findings) == 0 {
		t.Fatalf("no anomaly findings produced")
	}
	var sawFatal bool
	for _, f := range findings {
		if f.Severity == anomaly2.SeverityFatal {
			sawFatal = true
		}
	}
	if !sawFatal {
		t.Fatalf("expected fatal-severity finding, got %+v", findings)
	}
	if telegramCalls.Load() == 0 {
		t.Fatalf("telegram not called")
	}
	bodyStr, _ := telegramBody.Load().(string)
	if !strings.Contains(bodyStr, "Polymarket") {
		t.Errorf("telegram body missing branding:\n%s", bodyStr)
	}
}
