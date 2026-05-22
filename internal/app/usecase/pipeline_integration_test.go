// Package usecase_test wires real discover/collect/detect against
// httptest-backed upstreams. Nothing in here touches the public internet.
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

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/collect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/discover"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	anomaly2 "github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	alerting2 "github.com/Borislavv/polymarket-watchtower/internal/infra/alerting"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/gamma"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ratelimit"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
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

// TestPipelineDetectsWhalesAndCategoryWatch wires the full pipeline against
// fakes and asserts:
//   - per-trade anomalies fire for outsized single bets,
//   - a category-watch HARD alert fires when multiple wallets converge in one
//     category within the window,
//   - Telegram receives both kinds and the messages carry the expected fields
//     (links + dollar amounts + outcome + wallet).
func TestPipelineDetectsWhalesAndCategoryWatch(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// --- Gamma fake: 1 market, 1 tag, two outcomes (Yes/No) ----------------
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
					"conditionId":  "0xa",
					"slug":         "will-candidate-a-win",
					"question":     "Will Candidate A win?",
					"active":       true,
					"outcomes":     `["Yes","No"]`,
					"clobTokenIds": `["tok-yes","tok-no"]`,
					// Lifecycle: market started 95d ago and ends in 5d. Without
					// dates the detector silences everything by design (v4
					// fail-closed contract; no env override).
					"startDate": now.Add(-95 * 24 * time.Hour).Format(time.RFC3339),
					"endDate":   now.Add(5 * 24 * time.Hour).Format(time.RFC3339),
					// Parent event — the user-facing URL is /event/<event.slug>,
					// NOT /event/<market.slug>. Verified live: market slugs 404.
					"events": []map[string]any{{
						"id": "e1", "slug": "us-pres-2028", "title": "US Presidential Election 2028",
					}},
				}})
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer gammaSrv.Close()

	// --- Data API fake: baseline of small bets, then three whale bets by
	//     three distinct wallets within the cluster window --------------------
	type t1 struct {
		ID        string  `json:"id"`
		Cond      string  `json:"conditionId"`
		Asset     string  `json:"asset"`
		Side      string  `json:"side"`
		Size      float64 `json:"size"`
		Price     float64 `json:"price"`
		Timestamp int64   `json:"timestamp"`
		Wallet    string  `json:"proxyWallet"`
	}
	var trades []t1
	// 100 baseline "Yes" bets at notional $100 (above the $50 baseline filter).
	for i := 0; i < 100; i++ {
		trades = append(trades, t1{
			ID: "b" + strconv.Itoa(i), Cond: "0xa", Asset: "tok-yes",
			Side: "BUY", Size: 200, Price: 0.5, Wallet: "0xnoise",
			Timestamp: now.Add(-24 * time.Hour).Add(time.Duration(i*14) * time.Minute).Unix(),
		})
	}
	// 3 whales: $100k each at price 0.05 (odds 20). Absolute=critical.
	// Multiplier = 100000/100 = 1000 → warning. Conservative final = warning.
	// Cluster of 3 warning trades from 3 unique wallets totalling $300k fires HARD.
	for i, wallet := range []string{
		"0xshark111111111111111111111111111111111111",
		"0xshark222222222222222222222222222222222222",
		"0xshark333333333333333333333333333333333333",
	} {
		trades = append(trades, t1{
			ID: "w" + strconv.Itoa(i), Cond: "0xa", Asset: "tok-yes",
			Side: "BUY", Size: 2_000_000, Price: 0.05, Wallet: wallet,
			Timestamp: now.Add(-time.Duration(i) * time.Minute).Unix(),
		})
	}

	// Data API order is timestamp DESC within a market. Match the contract.
	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 500
		}
		// sort desc by timestamp on every call (cheap; small fixture)
		sorted := make([]t1, len(trades))
		copy(sorted, trades)
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].Timestamp > sorted[i].Timestamp {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		if offset >= len(sorted) {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		end := offset + limit
		if end > len(sorted) {
			end = len(sorted)
		}
		_ = json.NewEncoder(w).Encode(sorted[offset:end])
	}))
	defer dataSrv.Close()

	telegramCalls := atomic.Int32{}
	telegramBodies := make(chan string, 16)
	telegramSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		telegramCalls.Add(1)
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		select {
		case telegramBodies <- string(buf[:n]):
		default:
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegramSrv.Close()

	gh, _ := httpx.New(httpx.Config{BaseURL: gammaSrv.URL, Limiter: ratelimit.Noop{}})
	dh, _ := httpx.New(httpx.Config{BaseURL: dataSrv.URL, Limiter: ratelimit.Noop{}})
	gammaClient := gamma.New(gh)
	dataClient := dataapi.New(dh)

	met := metrics.New()
	reg := marketcache.New()
	log := zerolog.Nop()

	disc := discover.New(discover.Config{
		Interval: time.Hour, ActiveOnly: true, SafetyMaxMarkets: 100,
	}, gammaClient, reg, nil, met, &log)

	tg, err := alerting2.NewTelegramSink(alerting2.TelegramConfig{
		Enabled: true, BotToken: "test", ChatID: "1", BaseURL: telegramSrv.URL,
	})
	if err != nil {
		t.Fatalf("telegram sink: %v", err)
	}
	// v11.4: TelegramSink dispatches via the typed Router. Build a
	// dev-mode router with the test bot as the inner transport so the
	// httptest server receives the calls.
	telegramBot, err := telegram.New(telegram.Config{BotToken: "test", BaseURL: telegramSrv.URL})
	if err != nil {
		t.Fatalf("telegram bot: %v", err)
	}
	telegramRouter := telegram.NewRouter(
		telegram.RouterConfig{SignalEnabled: true, SignalChatID: "1"},
		telegramBot,
		nil,
	)
	tg = tg.WithSender(telegramRouter)
	cap := &capturingSink{}
	fanout := &alerting2.Fanout{Sinks: []alerting2.Channel{cap, tg}, Logger: &log}

	det := detect.New(detect.Config{
		Thresholds: anomaly2.Thresholds{
			Info: anomaly2.Tier{
				MinNotionalUSD: 10_000, MinOdds: 3,
				MinMarketP95Ratio: 1, MinTraderP95Ratio: 1,
				MinMultiplier: 100,
			},
			Warning: anomaly2.Tier{
				MinNotionalUSD: 25_000, MinOdds: 5,
				MinMarketP95Ratio: 2, MinTraderP95Ratio: 1.5,
				MinMultiplier: 1_000,
			},
			Critical: anomaly2.Tier{
				MinNotionalUSD: 100_000, MinOdds: 8,
				MinMarketP95Ratio: 4, MinTraderP95Ratio: 2,
				MinMultiplier: 10_000,
			},
			MinBaselineTrades:      20,
			MinBaselineNotionalUSD: 100,
		},
		Baseline: baseline.Config{Window: 7 * 24 * time.Hour},
		Cluster: cluster.Config{
			Window: time.Hour, MinTrades: 3, MinUniqueWallets: 3, MinTotalUSD: 50_000, Cooldown: time.Hour,
		},
		PolymarketBase: "https://polymarket.com",
		GrafanaBase:    "http://grafana.local",
		GrafanaDashUID: "uid1",
		GrafanaContext: time.Hour,
		Clock:          clock,
	}, reg, fanout, met, &log)

	collectLoop := collect.New(collect.Config{
		Interval: time.Hour, Concurrency: 1, BootstrapLookback: 25 * time.Hour, Clock: clock,
	}, dataClient, reg, det, met, &log)

	ctx := context.Background()
	if err := disc.RunOnce(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}
	collectLoop.Tick(ctx)

	// --- assert -----------------------------------------------------------
	if reg.Size() != 1 {
		t.Fatalf("registry size: %d", reg.Size())
	}
	findings := cap.Findings()
	if len(findings) == 0 {
		t.Fatal("no findings emitted")
	}

	var tradeAnoms, hardAlerts int
	var sawCritical, sawHard bool
	for _, f := range findings {
		switch f.Kind {
		case anomaly2.KindTradeAnomaly:
			tradeAnoms++
			// v5: each $100k whale clears every Critical gate (notional,
			// odds 20, market p95 ratio ~1000) → Critical severity.
			if f.Severity == anomaly2.SeverityCritical {
				sawCritical = true
			}
			if f.Trade == nil || f.Trade.Outcome != "Yes" {
				t.Errorf("trade ref missing outcome: %+v", f.Trade)
			}
			if f.MarketURL != "https://polymarket.com/event/us-pres-2028" {
				t.Errorf("market URL must point at the EVENT slug, got %q", f.MarketURL)
			}
			// Regression: never address the user at /event/<market-slug>.
			if strings.Contains(f.MarketURL, "/event/will-candidate-a-win") {
				t.Errorf("alert leaked a /event/<market-slug> URL: %q", f.MarketURL)
			}
			if !strings.Contains(f.GrafanaURL, "var-category=Politics") {
				t.Errorf("grafana URL missing category var: %q", f.GrafanaURL)
			}
		case anomaly2.KindCategoryWatch:
			hardAlerts++
			if f.Severity == anomaly2.SeverityHard {
				sawHard = true
			}
			if f.Cluster == nil || f.Cluster.UniqueWallets < 3 {
				t.Errorf("cluster stats: %+v", f.Cluster)
			}
		}
	}
	if tradeAnoms < 3 {
		t.Errorf("expected >=3 single-trade findings (one per whale), got %d", tradeAnoms)
	}
	if !sawCritical {
		t.Errorf("expected at least one critical single-trade finding")
	}
	if hardAlerts != 1 || !sawHard {
		t.Errorf("expected exactly 1 HARD category-watch alert, got %d", hardAlerts)
	}

	if telegramCalls.Load() == 0 {
		t.Fatal("telegram not called")
	}
	// At least one body should be the HARD alert with the required wording.
	close(telegramBodies)
	var sawWatchMsg bool
	for body := range telegramBodies {
		if strings.Contains(body, "CategoryWatchRequired") && strings.Contains(body, "3 unique traders") {
			sawWatchMsg = true
		}
	}
	if !sawWatchMsg {
		t.Error("telegram never received the CategoryWatchRequired message")
	}
}
