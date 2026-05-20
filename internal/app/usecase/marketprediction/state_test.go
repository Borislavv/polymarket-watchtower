package marketprediction

import (
	"strings"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func TestDecide_BlockedWhenActiveCatalyst(t *testing.T) {
	dec := Decide(Inputs{
		Now: time.Now().UTC(),
		Prediction: repository.MarketPrediction{
			CurrentState: "watching",
		},
		ActiveCatalysts: []repository.EventCatalyst{
			{Status: repository.CatalystStatusExpected, Title: "TX runoff", CatalystType: "runoff"},
		},
	}, Config{})
	if dec.NewState != StateBlocked {
		t.Errorf("expected blocked, got %q", dec.NewState)
	}
	if !dec.Changed {
		t.Errorf("changed must be true")
	}
}

func TestDecide_ResolvedCatalystOverridesEverything(t *testing.T) {
	dec := Decide(Inputs{
		Now:        time.Now().UTC(),
		Prediction: repository.MarketPrediction{CurrentState: "blocked"},
		ActiveCatalysts: []repository.EventCatalyst{
			{Status: repository.CatalystStatusResolved, Title: "TX runoff"},
		},
		MatchedAlerts: []MatchedAlert{
			{Score: 0.9, DirectionAlignment: "aligned"},
		},
	}, Config{})
	if dec.NewState != StateResolved {
		t.Errorf("expected resolved, got %q", dec.NewState)
	}
}

func TestDecide_ConfirmedByFlow_FromMatchedAlert(t *testing.T) {
	dec := Decide(Inputs{
		Now:        time.Now().UTC(),
		Prediction: repository.MarketPrediction{CurrentState: "watching", UpdatedAt: time.Now().UTC()},
		MatchedAlerts: []MatchedAlert{
			{Score: 0.75, DirectionAlignment: "aligned", Kind: "accumulation"},
		},
	}, Config{})
	if dec.NewState != StateConfirmedByFlow {
		t.Errorf("expected confirmed_by_flow, got %q", dec.NewState)
	}
}

func TestDecide_ContradictedByOppositeFlowImbalance(t *testing.T) {
	dec := Decide(Inputs{
		Now: time.Now().UTC(),
		Prediction: repository.MarketPrediction{
			CurrentState: "watching", UpdatedAt: time.Now().UTC(),
		},
		FlowSummary: eventflow.EventFlowSummary{
			RecentAlerts:         5,
			DirectionalImbalance: -0.85,
		},
	}, Config{})
	if dec.NewState != StateContradictedByFlow {
		t.Errorf("expected contradicted_by_flow, got %q (%s)", dec.NewState, dec.Reason)
	}
}

func TestDecide_AlreadyPriced_FromRepricingSignal(t *testing.T) {
	dec := Decide(Inputs{
		Now:        time.Now().UTC(),
		Prediction: repository.MarketPrediction{CurrentState: "watching", UpdatedAt: time.Now().UTC()},
		RepricingSignal: &repricing.Signal{
			RepricingStatus: repricing.StatusAlreadyPriced,
		},
	}, Config{})
	if dec.NewState != StateAlreadyPriced {
		t.Errorf("expected already_priced, got %q", dec.NewState)
	}
}

func TestDecide_StaleAfterTTL(t *testing.T) {
	old := time.Now().UTC().Add(-72 * time.Hour)
	dec := Decide(Inputs{
		Now: time.Now().UTC(),
		Prediction: repository.MarketPrediction{
			CurrentState:   "watching",
			UpdatedAt:      old,
			LastRepricedAt: old,
		},
	}, Config{StaleAfter: 24 * time.Hour})
	if dec.NewState != StateStale {
		t.Errorf("expected stale, got %q (%s)", dec.NewState, dec.Reason)
	}
}

func TestDecide_DefaultsToWatching(t *testing.T) {
	dec := Decide(Inputs{
		Now: time.Now().UTC(),
		Prediction: repository.MarketPrediction{
			CurrentState: "new",
			UpdatedAt:    time.Now().UTC(),
		},
	}, Config{})
	if dec.NewState != StateWatching {
		t.Errorf("expected watching, got %q", dec.NewState)
	}
}

func TestRenderTelegramBlock_StateOrderingHTMLEscape(t *testing.T) {
	pred := repository.MarketPrediction{CurrentState: "blocked", StateReason: "active catalyst"}
	dec := Decision{NewState: "confirmed_by_flow", PreviousState: "watching", Reason: "<bold>aligned</bold>"}
	out := RenderTelegramBlock(pred, dec)
	if !strings.Contains(out, "<b>Prediction state</b>") {
		t.Errorf("missing header: %s", out)
	}
	if strings.Contains(out, "<bold>aligned</bold>") {
		t.Errorf("unescaped HTML in reason: %s", out)
	}
	if !strings.Contains(out, "&lt;bold&gt;aligned&lt;/bold&gt;") {
		t.Errorf("expected HTML-escaped reason: %s", out)
	}
	if !strings.Contains(out, "changed from: watching") {
		t.Errorf("changed-from line missing: %s", out)
	}
}

// --- match scoring -------------------------------------------------------

func TestScore_AlignedAndIdentityYieldsHighScore(t *testing.T) {
	alert := AlertCandidate{
		AlertID: 1, Severity: anomaly.SeverityCritical, Kind: anomaly.KindTradeAnomaly,
		ConditionID: "0xa", EventSlug: "tx", Outcome: "Yes", Side: "BUY",
		At: time.Now().UTC(),
	}
	pred := PredictionRef{
		EventSlug: "tx", ConditionID: "0xa", Outcome: "Yes", SideBias: "BUY",
		CreatedAt: time.Now().UTC().Add(2 * time.Hour),
	}
	m := Score(alert, pred)
	if m.DirectionAlignment != "aligned" {
		t.Errorf("alignment: %q", m.DirectionAlignment)
	}
	if m.Score < 0.75 {
		t.Errorf("expected high score, got %.2f (reasons=%v)", m.Score, m.MatchedOn)
	}
}

func TestScore_OppositeSidePenalises(t *testing.T) {
	alert := AlertCandidate{
		AlertID: 1, Severity: anomaly.SeverityWarning, Kind: anomaly.KindAccumulation,
		ConditionID: "0xa", EventSlug: "tx", Outcome: "Yes", Side: "SELL",
		At: time.Now().UTC(),
	}
	pred := PredictionRef{
		EventSlug: "tx", ConditionID: "0xa", Outcome: "Yes", SideBias: "BUY",
		CreatedAt: time.Now().UTC().Add(2 * time.Hour),
	}
	m := Score(alert, pred)
	if m.DirectionAlignment != "contradict" {
		t.Errorf("alignment: %q", m.DirectionAlignment)
	}
}

func TestScore_DifferentEventReturnsLowScore(t *testing.T) {
	alert := AlertCandidate{
		ConditionID: "0xb", EventSlug: "ga", Outcome: "Yes", Side: "BUY",
	}
	pred := PredictionRef{EventSlug: "tx", ConditionID: "0xa"}
	m := Score(alert, pred)
	if m.Score > 0.2 {
		t.Errorf("expected low cross-event score, got %.2f", m.Score)
	}
}
