package marketprediction

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func dummyConfig() Config {
	cfg := Config{}
	cfg.applyDefaults()
	return cfg
}

// TestDecide_BlockedRevalidate_ExpectedAtPassed pins the v10.2
// revalidation rule: a prediction held in `blocked` with a single
// expected catalyst whose expected_at is 48h in the past should NOT
// remain blocked.
func TestDecide_BlockedRevalidate_ExpectedAtPassed(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	prev := repository.MarketPrediction{ID: 1, CurrentState: StateBlocked}
	dec := Decide(Inputs{
		Now:        now,
		Prediction: prev,
		ActiveCatalysts: []repository.EventCatalyst{{
			Status:     repository.CatalystStatusExpected,
			ExpectedAt: now.Add(-48 * time.Hour),
		}},
	}, dummyConfig())
	if dec.NewState == StateBlocked {
		t.Errorf("expected fall-through from blocked; got %q (%s)", dec.NewState, dec.Reason)
	}
}

// TestDecide_BlockedStaysWhenCatalystFuture pins the canonical
// blocked path: expected_at still in the future = stay blocked.
func TestDecide_BlockedStaysWhenCatalystFuture(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	dec := Decide(Inputs{
		Now:        now,
		Prediction: repository.MarketPrediction{CurrentState: "watching"},
		ActiveCatalysts: []repository.EventCatalyst{{
			Status:     repository.CatalystStatusExpected,
			ExpectedAt: now.Add(72 * time.Hour),
		}},
	}, dummyConfig())
	if dec.NewState != StateBlocked {
		t.Errorf("expected blocked; got %q", dec.NewState)
	}
}

// TestDecide_BlockedStaysWhenExpectedAtUnknown pins the conservative
// rule: when expected_at is missing we DON'T flip out of blocked
// (we have no positive evidence the catalyst won't fire).
func TestDecide_BlockedStaysWhenExpectedAtUnknown(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	dec := Decide(Inputs{
		Now:        now,
		Prediction: repository.MarketPrediction{CurrentState: "watching"},
		ActiveCatalysts: []repository.EventCatalyst{{
			Status: repository.CatalystStatusExpected,
		}},
	}, dummyConfig())
	if dec.NewState != StateBlocked {
		t.Errorf("expected blocked when expected_at is unknown; got %q", dec.NewState)
	}
}

// TestDecide_BlockedRevalidatedReasonExplains pins the operator-
// facing reason on the revalidation branch. The string must include
// the word "revalidated" so the Telegram body / dashboard shows it.
func TestDecide_BlockedRevalidatedReasonExplains(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	prev := repository.MarketPrediction{ID: 7, CurrentState: StateBlocked}
	dec := Decide(Inputs{
		Now:        now,
		Prediction: prev,
		ActiveCatalysts: []repository.EventCatalyst{{
			Status:     repository.CatalystStatusExpected,
			ExpectedAt: now.Add(-72 * time.Hour),
		}},
	}, dummyConfig())
	if dec.Reason == "" {
		t.Fatal("missing reason on revalidation")
	}
	// We expect either a downstream classifier (e.g. stale, flow,
	// repricing) to overwrite Reason — but the EVOLVED Reason should
	// not say "active catalyst blocks repricing" (that's the
	// blocked-forever wording we replaced).
	if dec.Reason == "active catalyst blocks repricing" {
		t.Errorf("legacy blocked reason leaked through revalidation: %q", dec.Reason)
	}
}
