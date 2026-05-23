package detect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/rulesrisk"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

type captureShadow struct {
	mu      sync.Mutex
	rows    []shadowdecisions.Decision
	allowed map[string]bool
}

func (c *captureShadow) ShouldEvaluate(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allowed[name]
}

func (c *captureShadow) Record(_ context.Context, d shadowdecisions.Decision) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, d)
	return int64(len(c.rows)), nil
}

func newLoopForShadow(sink *captureShadow, rr *rulesrisk.Detector) *Loop {
	log := zerolog.Nop()
	cfg := Config{
		Clock:                     func() time.Time { return time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC) },
		StrategyShadowBus:         sink,
		StrategyRulesRisk:         rr,
		StrategyShadowMaxPerTrade: 5,
	}
	// New requires a few optional fields; we don't exercise them on the
	// shadow path so leave them zero.
	return New(cfg, nil, nil, nil, &log)
}

func newFinding(catLabel string, wallet string, notional float64) anomaly.Finding {
	return anomaly.Finding{
		Kind: anomaly.KindTradeAnomaly,
		Trade: &anomaly.TradeRef{
			Wallet:      wallet,
			NotionalUSD: notional,
		},
		Category: &anomaly.CategoryRef{Label: catLabel},
	}
}

func TestRecordStrategyShadow_WritesRulesRiskRowForRiskyMarket(t *testing.T) {
	sink := &captureShadow{allowed: map[string]bool{"rulesrisk": true}}
	det := rulesrisk.New(rulesrisk.Config{})
	l := newLoopForShadow(sink, det)

	m := market.Market{
		ID:        vo.MarketID("cond-A"),
		Question:  "Will X be officially certified after runoff and court appeal?",
		EventSlug: "ny-mayor",
	}
	tr := trade.Trade{
		Token:     vo.TokenID("YES"),
		Side:      trade.SideBuy,
		Timestamp: time.Now(),
	}
	f := newFinding("Politics", "0xabc", 5000)
	n := l.recordStrategyShadow(context.Background(), m, tr, f, "dedup-1")
	if n != 1 {
		t.Fatalf("expected 1 shadow row; got %d", n)
	}
	if got, want := len(sink.rows), 1; got != want {
		t.Fatalf("sink rows: got %d want %d", got, want)
	}
	row := sink.rows[0]
	if row.StrategyName != "rulesrisk" {
		t.Fatalf("expected rulesrisk row; got %q", row.StrategyName)
	}
	if row.LinkedAlertDedupKey != "dedup-1" {
		t.Fatalf("expected linked dedup; got %q", row.LinkedAlertDedupKey)
	}
	if row.Kind != shadowdecisions.KindTag {
		t.Fatalf("expected tag; got %q", row.Kind)
	}
}

func TestRecordStrategyShadow_NoMarkersSkipsByDefault(t *testing.T) {
	sink := &captureShadow{allowed: map[string]bool{"rulesrisk": true}}
	det := rulesrisk.New(rulesrisk.Config{})
	l := newLoopForShadow(sink, det)

	m := market.Market{
		ID:       vo.MarketID("cond-A"),
		Question: "Will Y resolve YES?",
	}
	tr := trade.Trade{Token: vo.TokenID("YES")}
	f := newFinding("Politics", "0xabc", 1000)
	n := l.recordStrategyShadow(context.Background(), m, tr, f, "dedup-1")
	if n != 0 {
		t.Fatalf("expected 0 shadow rows on bland title; got %d", n)
	}
}

func TestRecordStrategyShadow_DisabledBusNoOp(t *testing.T) {
	l := &Loop{}
	n := l.recordStrategyShadow(context.Background(), market.Market{}, trade.Trade{}, anomaly.Finding{}, "dedup-1")
	if n != 0 {
		t.Fatalf("expected 0 when bus disabled; got %d", n)
	}
}

func TestRecordStrategyShadow_BusReturnsErrorIsLoggedNotPanic(t *testing.T) {
	// Bus that returns an error on Record. Should not crash, should
	// still report 0 rows written.
	bus := &errorBus{}
	det := rulesrisk.New(rulesrisk.Config{})
	l := newLoopForShadow(nil, det)
	l.cfg.StrategyShadowBus = bus

	m := market.Market{ID: vo.MarketID("cond-A"), Question: "Will X be officially certified after runoff?"}
	tr := trade.Trade{Token: vo.TokenID("YES")}
	f := newFinding("Politics", "0xabc", 5000)
	n := l.recordStrategyShadow(context.Background(), m, tr, f, "dedup-1")
	if n != 0 {
		t.Fatalf("write_failed should report 0 rows; got %d", n)
	}
	if !bus.called {
		t.Fatalf("bus.Record was not called")
	}
}

type errorBus struct {
	called bool
}

func (e *errorBus) ShouldEvaluate(_ string) bool { return true }
func (e *errorBus) Record(_ context.Context, _ shadowdecisions.Decision) (int64, error) {
	e.called = true
	return 0, context.DeadlineExceeded
}
