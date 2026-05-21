package concentration

import (
	"context"
	"testing"
	"time"
)

// fakeHistory is a tiny in-memory AlertHistory for unit tests.
type fakeHistory struct {
	byEvent  map[string][]Alert
	byWallet map[string][]Alert
}

func (f *fakeHistory) RecentAlertsForEvent(_ context.Context, slug string, since time.Time) ([]Alert, error) {
	return filterAfter(f.byEvent[slug], since), nil
}
func (f *fakeHistory) RecentAlertsForWallet(_ context.Context, wallet, slug string, since time.Time) ([]Alert, error) {
	return filterAfter(f.byWallet[wallet+"|"+slug], since), nil
}

func filterAfter(rows []Alert, since time.Time) []Alert {
	out := make([]Alert, 0, len(rows))
	for _, r := range rows {
		if r.CreatedAt.After(since) {
			out = append(out, r)
		}
	}
	return out
}

// First-ever alert on a wallet/event must pass (no priors).
func TestEvaluate_FirstAlertPasses(t *testing.T) {
	g := New(Config{})
	d, _ := g.Evaluate(context.Background(),
		Candidate{EventSlug: "tx", Wallet: "0xa", NotionalUSD: 3000, Severity: "info", Now: time.Now()},
		&fakeHistory{},
	)
	if !d.Allow {
		t.Errorf("first alert must pass; got %+v", d)
	}
}

// Same wallet, same event, smaller notional within cooldown → SUPPRESS.
//
// This pins the v10.8 wallet-escalation rule: an operator should
// never receive the same wallet's $3k alert immediately after the
// $14k alert on the same event — the v10.8 audit found this exact
// pattern (us-x-iran wallet `0xc0402…beac` fired 4× in 17 minutes).
func TestEvaluate_WalletEscalation(t *testing.T) {
	hist := &fakeHistory{
		byWallet: map[string][]Alert{
			"0xa|tx": {
				{CreatedAt: time.Now().Add(-1 * time.Hour), NotionalUSD: 14000, EventSlug: "tx", Wallet: "0xa", Severity: "info"},
			},
		},
	}
	g := New(Config{WalletAlertCooldown: 6 * time.Hour, AccumulationEscalationFactor: 2})
	d, _ := g.Evaluate(context.Background(),
		Candidate{EventSlug: "tx", Wallet: "0xa", NotionalUSD: 3000, Severity: "info", Now: time.Now()},
		hist,
	)
	if d.Allow {
		t.Errorf("second wallet alert with smaller notional must be suppressed; got %+v", d)
	}
	if d.Reason != "wallet_escalation_failed" {
		t.Errorf("reason: got %q want wallet_escalation_failed", d.Reason)
	}
	if d.RequiredNotional != 28000 {
		t.Errorf("required: got %.0f want 28000", d.RequiredNotional)
	}
}

// Same wallet, same event, LARGER notional (2× previous) → PASS.
func TestEvaluate_WalletEscalationBypassedByLargerNotional(t *testing.T) {
	hist := &fakeHistory{
		byWallet: map[string][]Alert{
			"0xa|tx": {
				{CreatedAt: time.Now().Add(-1 * time.Hour), NotionalUSD: 5000},
			},
		},
	}
	g := New(Config{WalletAlertCooldown: 6 * time.Hour, AccumulationEscalationFactor: 2})
	d, _ := g.Evaluate(context.Background(),
		Candidate{EventSlug: "tx", Wallet: "0xa", NotionalUSD: 15000, Severity: "info", Now: time.Now()},
		hist,
	)
	if !d.Allow {
		t.Errorf("larger notional must pass escalation; got %+v", d)
	}
}

// Event-concentration cap: ≥3 alerts on the same event in window →
// next alert must clear 2× previous max.
func TestEvaluate_EventConcentrationCap(t *testing.T) {
	now := time.Now()
	prior := []Alert{
		{CreatedAt: now.Add(-6 * time.Hour), NotionalUSD: 5000, EventSlug: "tx"},
		{CreatedAt: now.Add(-4 * time.Hour), NotionalUSD: 8000, EventSlug: "tx"},
		{CreatedAt: now.Add(-2 * time.Hour), NotionalUSD: 12000, EventSlug: "tx"},
	}
	hist := &fakeHistory{byEvent: map[string][]Alert{"tx": prior}}
	g := New(Config{
		EventConcentrationLimit:          3,
		EventConcentrationWindow:         24 * time.Hour,
		RepeatedEventThresholdMultiplier: 2,
		WalletAlertCooldown:              6 * time.Hour,
		AccumulationEscalationFactor:     2,
	})
	d, _ := g.Evaluate(context.Background(),
		Candidate{EventSlug: "tx", Wallet: "0xb", NotionalUSD: 15000, Severity: "info", Now: now},
		hist,
	)
	if d.Allow {
		t.Errorf("4th alert below 2× prev_max must suppress; got %+v", d)
	}
	if d.RequiredNotional != 24000 {
		t.Errorf("required: got %.0f want 24000", d.RequiredNotional)
	}
}

// Warning/Critical bypass concentration entirely.
func TestEvaluate_WarningBypassesGate(t *testing.T) {
	hist := &fakeHistory{
		byEvent: map[string][]Alert{"tx": {
			{CreatedAt: time.Now().Add(-1 * time.Hour), NotionalUSD: 50000},
			{CreatedAt: time.Now().Add(-2 * time.Hour), NotionalUSD: 50000},
			{CreatedAt: time.Now().Add(-3 * time.Hour), NotionalUSD: 50000},
		}},
	}
	g := New(Config{EventConcentrationLimit: 1})
	d, _ := g.Evaluate(context.Background(),
		Candidate{EventSlug: "tx", NotionalUSD: 5000, Severity: "warning", Now: time.Now()},
		hist,
	)
	if !d.Allow {
		t.Errorf("warning must bypass concentration; got %+v", d)
	}
}

// Old alerts outside the cooldown window do NOT count.
func TestEvaluate_StalePriorsIgnored(t *testing.T) {
	hist := &fakeHistory{
		byWallet: map[string][]Alert{
			"0xa|tx": {
				{CreatedAt: time.Now().Add(-48 * time.Hour), NotionalUSD: 50000},
			},
		},
	}
	g := New(Config{WalletAlertCooldown: 6 * time.Hour, AccumulationEscalationFactor: 2})
	d, _ := g.Evaluate(context.Background(),
		Candidate{EventSlug: "tx", Wallet: "0xa", NotionalUSD: 3000, Severity: "info", Now: time.Now()},
		hist,
	)
	if !d.Allow {
		t.Errorf("stale prior must not count; got %+v", d)
	}
}

func TestEvaluate_DisabledGate(t *testing.T) {
	var g *Gate
	d, err := g.Evaluate(context.Background(), Candidate{}, nil)
	if err != nil {
		t.Errorf("nil gate must not error: %v", err)
	}
	if !d.Allow {
		t.Errorf("nil gate must allow")
	}
}
