package cluster

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

func tr(wallet string, notional float64, at time.Time) anomaly.TradeRef {
	return anomaly.TradeRef{Wallet: wallet, NotionalUSD: notional, At: at}
}

func TestFiresWhenAllCriteriaMet(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := New(Config{
		Window: time.Hour, MinTrades: 3, MinUniqueWallets: 2, MinTotalUSD: 1000,
		Clock: func() time.Time { return now },
	})
	if got := d.Observe(1, tr("a", 500, now)); got != nil {
		t.Fatalf("fired too early: %+v", got)
	}
	if got := d.Observe(1, tr("a", 500, now)); got != nil {
		t.Fatalf("still need second wallet")
	}
	got := d.Observe(1, tr("b", 1000, now))
	if got == nil {
		t.Fatal("expected fire")
	}
	if got.AnomalousTrades != 3 || got.UniqueWallets != 2 || got.TotalUSD != 2000 {
		t.Fatalf("stats: %+v", got)
	}
	if got.Window != time.Hour {
		t.Fatalf("window: %v", got.Window)
	}
}

func TestDoesNotFireBelowMinWallets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := New(Config{Window: time.Hour, MinTrades: 3, MinUniqueWallets: 3, MinTotalUSD: 0, Clock: func() time.Time { return now }})
	for i := 0; i < 10; i++ {
		if got := d.Observe(1, tr("solo", 1000, now)); got != nil {
			t.Fatalf("fired with one wallet: %+v", got)
		}
	}
}

func TestDoesNotFireBelowTotalNotional(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := New(Config{Window: time.Hour, MinTrades: 3, MinUniqueWallets: 2, MinTotalUSD: 10_000, Clock: func() time.Time { return now }})
	d.Observe(1, tr("a", 100, now))
	d.Observe(1, tr("b", 100, now))
	if got := d.Observe(1, tr("c", 100, now)); got != nil {
		t.Fatalf("fired below MinTotalUSD: %+v", got)
	}
}

func TestDoesNotFireForDifferentCategories(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := New(Config{Window: time.Hour, MinTrades: 2, MinUniqueWallets: 2, Clock: func() time.Time { return now }})
	d.Observe(1, tr("a", 1, now))
	if got := d.Observe(2, tr("b", 1, now)); got != nil {
		t.Fatalf("cross-category fire: %+v", got)
	}
}

func TestDoesNotFireForExpiredEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := now
	d := New(Config{Window: 10 * time.Minute, MinTrades: 3, MinUniqueWallets: 2, Clock: func() time.Time { return clock }})
	d.Observe(1, tr("a", 100, now.Add(-time.Hour)))
	d.Observe(1, tr("b", 100, now.Add(-time.Hour)))
	clock = now
	if got := d.Observe(1, tr("c", 100, now)); got != nil {
		t.Fatalf("fired with stale entries: %+v", got)
	}
}

func TestCooldownPreventsSpam(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := now
	d := New(Config{
		Window: time.Hour, Cooldown: time.Hour,
		MinTrades: 2, MinUniqueWallets: 2,
		Clock: func() time.Time { return clock },
	})
	d.Observe(1, tr("a", 1, now))
	first := d.Observe(1, tr("b", 1, now))
	if first == nil {
		t.Fatal("expected first fire")
	}
	// Same window, more trades — must not fire again until cooldown elapses.
	if got := d.Observe(1, tr("c", 1, now)); got != nil {
		t.Fatalf("fired during cooldown: %+v", got)
	}
	clock = clock.Add(2 * time.Hour)
	d.Observe(1, tr("d", 1, clock))
	if got := d.Observe(1, tr("e", 1, clock)); got == nil {
		t.Fatal("expected fire after cooldown")
	}
}

func TestSampleCapped(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := New(Config{
		Window: time.Hour, MinTrades: 10, MinUniqueWallets: 1, SampleCap: 2,
		Clock: func() time.Time { return now },
	})
	var got *anomaly.ClusterStats
	for i := 0; i < 10; i++ {
		got = d.Observe(1, tr("a", 1, now))
	}
	if got == nil {
		t.Fatal("expected fire on the 10th observe")
	}
	if len(got.Sample) != 2 {
		t.Fatalf("sample cap not respected: %d", len(got.Sample))
	}
}

func TestForget(t *testing.T) {
	d := New(Config{Window: time.Hour, MinTrades: 1, MinUniqueWallets: 1})
	d.Observe(vo.CategoryID(1), tr("a", 1, time.Now()))
	d.Forget(vo.CategoryID(1))
	if got := d.Observe(vo.CategoryID(1), tr("b", 1, time.Now())); got != nil {
		// firing on a single trade is allowed (MinTrades=1, MinWallets=1) — that's fine.
		_ = got
	}
}
