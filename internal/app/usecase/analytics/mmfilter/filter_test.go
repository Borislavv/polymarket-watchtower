package mmfilter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeTraders struct {
	byWallet map[string]repository.Trader
}

func (f *fakeTraders) GetByWallet(_ context.Context, w string) (repository.Trader, error) {
	t, ok := f.byWallet[w]
	if !ok {
		return repository.Trader{}, repository.ErrTraderNotFound
	}
	return t, nil
}

type fakeActivity struct {
	act   repository.TraderSideActivity
	err   error
	calls int
}

func (f *fakeActivity) SideActivity(_ context.Context, _, _ int64, _ string, _ time.Time) (repository.TraderSideActivity, error) {
	f.calls++
	return f.act, f.err
}

func defaultCfg() Config {
	return Config{
		Enabled:          true,
		Lookback:         24 * time.Hour,
		MinTradesPerSide: 4,
		NeutralityTol:    0.3,
	}
}

func TestFilter_DisabledAlwaysPasses(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false
	f := New(cfg, nil, nil)
	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if v.Suppress {
		t.Fatal("disabled filter must never suppress")
	}
}

func TestFilter_UnknownWalletPasses(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{}}
	act := &fakeActivity{}
	f := New(defaultCfg(), tr, act)

	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if v.Suppress {
		t.Fatal("unknown trader must pass")
	}
	if act.calls != 0 {
		t.Fatal("activity must not be queried for unknown trader")
	}
}

func TestFilter_OneSidedActivityPasses(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	act := &fakeActivity{act: repository.TraderSideActivity{
		BuyCount: 10, SellCount: 0,
		BuyNotionalUSD: 100_000, SellNotionalUSD: 0,
	}}
	f := New(defaultCfg(), tr, act)

	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if v.Suppress {
		t.Fatalf("one-sided activity must NOT be suppressed: %+v", v)
	}
}

func TestFilter_ThinTwoSidedPasses(t *testing.T) {
	// 3 BUY and 5 SELL — BUY side is below MinTradesPerSide=4. Pass.
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	act := &fakeActivity{act: repository.TraderSideActivity{
		BuyCount: 3, SellCount: 5,
		BuyNotionalUSD: 5_000, SellNotionalUSD: 5_000,
	}}
	f := New(defaultCfg(), tr, act)

	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if v.Suppress {
		t.Fatalf("thin one-side activity must NOT be suppressed: %+v", v)
	}
}

func TestFilter_BalancedTwoSidedSuppresses(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	act := &fakeActivity{act: repository.TraderSideActivity{
		BuyCount: 8, SellCount: 9,
		BuyNotionalUSD: 100_000, SellNotionalUSD: 110_000,
		// imbalance = 10000/110000 ≈ 0.091, well under 0.3 tol
	}}
	f := New(defaultCfg(), tr, act)

	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !v.Suppress {
		t.Fatalf("balanced two-sided activity must be suppressed: %+v", v)
	}
	if v.Imbalance > 0.1 {
		t.Errorf("imbalance: %v want ~0.09", v.Imbalance)
	}
}

func TestFilter_DirectionalBiasOverTolPasses(t *testing.T) {
	// 8 BUY/$200k vs 5 SELL/$50k — clear directional bias. Imbalance =
	// (200-50)/200 = 0.75 > 0.3 → pass.
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	act := &fakeActivity{act: repository.TraderSideActivity{
		BuyCount: 8, SellCount: 5,
		BuyNotionalUSD: 200_000, SellNotionalUSD: 50_000,
	}}
	f := New(defaultCfg(), tr, act)

	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if v.Suppress {
		t.Fatalf("directional bias must NOT be suppressed: %+v", v)
	}
	if v.Imbalance < 0.7 {
		t.Errorf("imbalance: %v want ~0.75", v.Imbalance)
	}
}

func TestFilter_ActivityErrorFailsOpen(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	act := &fakeActivity{err: errors.New("db hiccup")}
	f := New(defaultCfg(), tr, act)

	v, err := f.Decide(context.Background(), "0xa", 1, "tok")
	if err == nil {
		t.Fatal("expected error to propagate so caller can log")
	}
	if v.Suppress {
		t.Fatal("DB error must NOT cause suppression — fail-open")
	}
}

func TestFilter_EmptyArgsPass(t *testing.T) {
	tr := &fakeTraders{byWallet: map[string]repository.Trader{"0xa": {ID: 1}}}
	act := &fakeActivity{}
	f := New(defaultCfg(), tr, act)

	cases := []struct {
		wallet  string
		market  int64
		outcome string
	}{
		{"", 1, "tok"},
		{"0xa", 0, "tok"},
		{"0xa", 1, ""},
	}
	for _, c := range cases {
		v, err := f.Decide(context.Background(), c.wallet, c.market, c.outcome)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if v.Suppress {
			t.Fatalf("empty args must pass: %+v", v)
		}
	}
	if act.calls != 0 {
		t.Fatal("empty args must not reach the DB")
	}
}

func TestImbalanceMath(t *testing.T) {
	cases := []struct {
		buy, sell, want float64
	}{
		{0, 0, 0},
		{100, 0, 1},
		{0, 100, 1},
		{100, 100, 0},
		{100, 90, 0.1},
		{200, 50, 0.75},
	}
	for _, c := range cases {
		got := imbalance(c.buy, c.sell)
		if got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("imbalance(%v, %v): got %v want %v", c.buy, c.sell, got, c.want)
		}
	}
}
