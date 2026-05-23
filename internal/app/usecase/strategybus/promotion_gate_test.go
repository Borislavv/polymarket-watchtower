package strategybus

import (
	"context"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
)

type denyAll struct{}

func (denyAll) Allow(_ string) bool { return false }

type allowAll struct{}

func (allowAll) Allow(_ string) bool { return true }

// v11.6: even when ALL operator flags say "go live", the gate's
// denial forces ShadowOnly=true. This is the ТЗ guarantee:
// "operator flag alone is insufficient".
func TestRecord_PromotionGateDeniesForcesShadow(t *testing.T) {
	w := &captureWriter{}
	cfg := newCfg()
	cfg.GlobalPromotionAllowed = true
	cfg.Flags["thesisaccum"] = StrategyFlag{Name: "thesisaccum", Enabled: true, ShadowOnly: false}
	cfg.PromotionGate = denyAll{}
	bus := New(cfg, w, nil, nil)
	_, _ = bus.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindStandalone,
		ShadowOnly:   false,
	})
	if !w.rows[0].ShadowOnly {
		t.Fatalf("gate.deny must force ShadowOnly=true even when all other flags allow")
	}
}

func TestRecord_LivePassesWhenAllFlagsAndGateAllow(t *testing.T) {
	w := &captureWriter{}
	cfg := newCfg()
	cfg.GlobalPromotionAllowed = true
	cfg.Flags["thesisaccum"] = StrategyFlag{Name: "thesisaccum", Enabled: true, ShadowOnly: false}
	cfg.PromotionGate = allowAll{}
	bus := New(cfg, w, nil, nil)
	_, _ = bus.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindStandalone,
		ShadowOnly:   false,
	})
	if w.rows[0].ShadowOnly {
		t.Fatalf("live path must be reachable when gate allows + flags allow")
	}
}
