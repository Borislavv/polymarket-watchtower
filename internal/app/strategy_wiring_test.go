package app

import (
	"context"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
)

func TestBuildStrategyBus_DefaultsBlockAllWrites(t *testing.T) {
	// Defaults from the env-tag block: every detector disabled.
	var s StrategyConfig
	bus := BuildStrategyBus(s, shadowdecisions.NopWriter{}, nil)
	for _, name := range []string{
		"thesisaccum", "holderdelta", "catalystwindow", "bookvacuum",
		"repricinglag", "walletcohort", "conflictresolve", "rulesrisk", "cheaptail",
	} {
		if bus.ShouldEvaluate(name) {
			t.Fatalf("strategy %s must default disabled", name)
		}
	}
	id, err := bus.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindStandalone,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id != 0 {
		t.Fatalf("disabled strategy must skip writes; got id=%d", id)
	}
}
