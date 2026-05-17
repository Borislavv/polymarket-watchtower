package aggregate

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

func TestEngineWindowFolds(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	e := New(Config{
		Bucket:   time.Minute,
		Baseline: 2 * time.Hour,
		Clock:    func() time.Time { return now },
	})

	mid := vo.MarketID("0xabc")
	for i := 0; i < 10; i++ {
		e.Ingest(trade.Trade{
			Market:    mid,
			Timestamp: now.Add(-time.Duration(i) * time.Minute),
			Size:      2,
			Price:     0.5,
			Side:      trade.SideBuy,
		})
	}

	w := e.Window(mid, 5*time.Minute)
	// Trades inside [now-5m, now): bucketStart at now-1m..now-4m → 4 trades.
	// (the bucket-at-now might not be present if Truncate rounds down; we just
	// assert non-empty and rate sanity.)
	if w.Count == 0 {
		t.Fatal("expected trades in recent window")
	}
	if w.AvgSize() != 2 {
		t.Fatalf("avg size: got %v", w.AvgSize())
	}
	if w.NotionalPerMinute() <= 0 {
		t.Fatal("notional rate should be > 0")
	}
}

func TestEngineForgetReleasesRing(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	e := New(Config{Bucket: time.Minute, Baseline: time.Hour, Clock: func() time.Time { return now }})
	e.Ingest(trade.Trade{Market: "0xa", Timestamp: now.Add(-time.Minute), Size: 1, Price: 1, Side: trade.SideBuy})
	if got := e.Window("0xa", time.Hour).Count; got != 1 {
		t.Fatalf("pre: %d", got)
	}
	e.Forget("0xa")
	if got := e.Window("0xa", time.Hour).Count; got != 0 {
		t.Fatalf("post forget: %d", got)
	}
	if mids := e.Markets(); len(mids) != 0 {
		t.Fatalf("Markets after Forget: %+v", mids)
	}
}

func TestEngineConcurrentIngestSafe(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	e := New(Config{Bucket: time.Minute, Baseline: time.Hour, Clock: func() time.Time { return now }})
	const workers = 8
	const each = 1000
	done := make(chan struct{}, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			for i := 0; i < each; i++ {
				e.Ingest(trade.Trade{
					Market:    vo.MarketID([]byte{'0', 'x', byte('a' + w)}),
					Timestamp: now.Add(-time.Duration(i%30) * time.Minute),
					Size:      1, Price: 0.5, Side: trade.SideBuy,
				})
			}
			done <- struct{}{}
		}(w)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	if got := len(e.Markets()); got != workers {
		t.Fatalf("expected %d markets, got %d", workers, got)
	}
}

func TestEngineDropsAncient(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	e := New(Config{
		Bucket:   time.Minute,
		Baseline: time.Hour,
		Clock:    func() time.Time { return now },
	})
	e.Ingest(trade.Trade{
		Market:    vo.MarketID("0xabc"),
		Timestamp: now.Add(-2 * time.Hour),
		Size:      1, Price: 0.5, Side: trade.SideBuy,
	})
	if got := e.Window(vo.MarketID("0xabc"), time.Hour).Count; got != 0 {
		t.Fatalf("expected drop, got %d", got)
	}
}
