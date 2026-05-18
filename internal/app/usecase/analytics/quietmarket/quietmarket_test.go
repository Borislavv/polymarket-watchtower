package quietmarket

import (
	"testing"
	"time"
)

func cfg() Config {
	return Config{
		Enabled:               true,
		MaxTradesPerDay:       10,
		MaxNotionalPerDayUSD:  5_000,
		MinIdleDuration:       6 * time.Hour,
		MinCurrentNotionalUSD: 10_000,
		MinMultiplier:         50,
	}
}

func now() time.Time { return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) }

// TestDecide_QuietMarketLargeTradeQualifies pins the canonical case: 30
// days of history with 30 trades totalling $3000 (1 trade/day, $100/day),
// last trade 12h ago, $25k bet now → qualifies.
func TestDecide_QuietMarketLargeTradeQualifies(t *testing.T) {
	d := New(cfg())
	hist := History{
		SampleCount:      30,
		TotalNotionalUSD: 3_000,
		Span:             30 * 24 * time.Hour,
		MarketMedianUSD:  100,
		LastTradedAt:     now().Add(-12 * time.Hour),
	}
	event := Event{NotionalUSD: 25_000, At: now()}

	v := d.Decide(hist, event)
	if !v.Qualifies {
		t.Fatalf("expected wake-up, got %+v", v)
	}
	if v.Reason != ReasonQuietMarketWakeup {
		t.Errorf("reason: got %q want %s", v.Reason, ReasonQuietMarketWakeup)
	}
	if v.TradesPerDay > 1.1 || v.TradesPerDay < 0.9 {
		t.Errorf("tradesPerDay: got %v want ~1", v.TradesPerDay)
	}
	if v.NotionalPerDayUSD < 99 || v.NotionalPerDayUSD > 101 {
		t.Errorf("notionalPerDay: got %v want ~100", v.NotionalPerDayUSD)
	}
	if v.IdleDuration != 12*time.Hour {
		t.Errorf("idle: got %s want 12h", v.IdleDuration)
	}
}

// TestDecide_ActiveMarketDoesNotQualify pins the inverse: market with 500
// trades over 30 days totalling $500k (16 trades/day, $16k/day) — both
// ceilings exceeded → not quiet.
func TestDecide_ActiveMarketDoesNotQualify(t *testing.T) {
	d := New(cfg())
	hist := History{
		SampleCount:      500,
		TotalNotionalUSD: 500_000,
		Span:             30 * 24 * time.Hour,
		MarketMedianUSD:  500,
		LastTradedAt:     now().Add(-30 * time.Minute),
	}
	event := Event{NotionalUSD: 25_000, At: now()}

	v := d.Decide(hist, event)
	if v.Qualifies {
		t.Fatalf("active market must not qualify: %+v", v)
	}
	if v.Reason != "" {
		t.Errorf("reason must be empty when not qualifying: %q", v.Reason)
	}
}

// TestDecide_TinyTradeDoesNotQualify pins the per-event floor.
func TestDecide_TinyTradeDoesNotQualify(t *testing.T) {
	d := New(cfg())
	hist := History{
		SampleCount: 30, TotalNotionalUSD: 3_000,
		Span: 30 * 24 * time.Hour, MarketMedianUSD: 100,
		LastTradedAt: now().Add(-12 * time.Hour),
	}
	event := Event{NotionalUSD: 500, At: now()}

	if v := d.Decide(hist, event); v.Qualifies {
		t.Fatalf("tiny trade must not trigger quiet-market: %+v", v)
	}
}

// TestDecide_InsufficientBaselineDoesNotQualify pins the readiness gate.
// Without observable history we cannot judge "quiet".
func TestDecide_InsufficientBaselineDoesNotQualify(t *testing.T) {
	d := New(cfg())
	hist := History{SampleCount: 0, Span: 0}
	event := Event{NotionalUSD: 25_000, At: now()}

	if v := d.Decide(hist, event); v.Qualifies {
		t.Fatalf("zero baseline must not qualify: %+v", v)
	}
}

// TestDecide_DisabledNeverQualifies pins the master switch.
func TestDecide_DisabledNeverQualifies(t *testing.T) {
	c := cfg()
	c.Enabled = false
	d := New(c)
	hist := History{
		SampleCount: 30, TotalNotionalUSD: 3_000,
		Span: 30 * 24 * time.Hour, MarketMedianUSD: 100,
		LastTradedAt: now().Add(-12 * time.Hour),
	}
	if v := d.Decide(hist, Event{NotionalUSD: 25_000, At: now()}); v.Qualifies {
		t.Fatal("disabled detector must never qualify")
	}
}

// TestDecide_IdleFloorBlocksFastFollowUp pins: even a $25k bet on a quiet
// market is NOT a wake-up when there was another trade 30 minutes ago —
// the market is "warm enough" right now.
func TestDecide_IdleFloorBlocksFastFollowUp(t *testing.T) {
	d := New(cfg())
	hist := History{
		SampleCount: 30, TotalNotionalUSD: 3_000,
		Span: 30 * 24 * time.Hour, MarketMedianUSD: 100,
		LastTradedAt: now().Add(-30 * time.Minute),
	}
	if v := d.Decide(hist, Event{NotionalUSD: 25_000, At: now()}); v.Qualifies {
		t.Fatalf("fast follow-up must not be wake-up: %+v", v)
	}
}

// TestDecide_MultiplierGateBlocksOrdinaryBet pins: a $1k bet on a quiet
// market whose median is $200 only has multiplier 5 — below the
// MinMultiplier=50 floor, so it's normal trickle activity, not a wake-up.
func TestDecide_MultiplierGateBlocksOrdinaryBet(t *testing.T) {
	c := cfg()
	c.MinCurrentNotionalUSD = 500 // lower so the event isn't rejected by size
	d := New(c)
	hist := History{
		SampleCount: 30, TotalNotionalUSD: 3_000,
		Span: 30 * 24 * time.Hour, MarketMedianUSD: 200,
		LastTradedAt: now().Add(-12 * time.Hour),
	}
	if v := d.Decide(hist, Event{NotionalUSD: 1_000, At: now()}); v.Qualifies {
		t.Fatalf("trade under multiplier floor must not qualify: %+v", v)
	}
}

// TestDecide_NoPriorTradeStillQualifies pins the edge case where the
// outcome has NEVER traded before the event (LastTradedAt zero). That is
// the strongest possible quiet signal and the detector treats it as
// passing the idle gate by default.
func TestDecide_NoPriorTradeStillQualifies(t *testing.T) {
	d := New(cfg())
	hist := History{
		SampleCount: 1, TotalNotionalUSD: 100,
		Span:            48 * time.Hour, // some span (the trade itself is "old")
		MarketMedianUSD: 100,
		LastTradedAt:    time.Time{}, // no prior — the event is the second-ever trade
	}
	v := d.Decide(hist, Event{NotionalUSD: 25_000, At: now()})
	if !v.Qualifies {
		t.Fatalf("no-prior-trade case must qualify: %+v", v)
	}
	if v.IdleDuration != 0 {
		t.Errorf("idle should be zero when no prior trade: %s", v.IdleDuration)
	}
}

// TestDecide_NotionalCeilingAlone pins: a market with 1 trade/day (under
// trade ceiling) but $10k/day (over notional ceiling) is NOT quiet.
// Both ceilings must hold.
func TestDecide_NotionalCeilingAlone(t *testing.T) {
	d := New(cfg())
	hist := History{
		SampleCount: 30, TotalNotionalUSD: 300_000, // $10k/day
		Span: 30 * 24 * time.Hour, MarketMedianUSD: 5_000,
		LastTradedAt: now().Add(-12 * time.Hour),
	}
	if v := d.Decide(hist, Event{NotionalUSD: 250_000, At: now()}); v.Qualifies {
		t.Fatalf("dollar-active market must not qualify: %+v", v)
	}
}
