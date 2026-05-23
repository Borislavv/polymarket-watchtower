package strategybus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
)

type captureWriter struct {
	mu       sync.Mutex
	rows     []shadowdecisions.Decision
	failNext bool
}

func (c *captureWriter) Record(_ context.Context, d shadowdecisions.Decision) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext {
		c.failNext = false
		return 0, errors.New("synthetic failure")
	}
	c.rows = append(c.rows, d)
	return int64(len(c.rows)), nil
}

func newCfg() Config {
	return Config{
		StrategyVersion:        "v11.5-test",
		GlobalPromotionAllowed: false,
		Flags: map[string]StrategyFlag{
			"thesisaccum":     {Name: "thesisaccum", Enabled: true, ShadowOnly: true},
			"holderdelta":     {Name: "holderdelta", Enabled: true, ShadowOnly: true},
			"catalystwindow":  {Name: "catalystwindow", Enabled: false, ShadowOnly: true},
			"conflictresolve": {Name: "conflictresolve", Enabled: true, ShadowOnly: false},
		},
	}
}

func TestRecord_WritesShadowRow(t *testing.T) {
	w := &captureWriter{}
	b := New(newCfg(), w, nil, nil)
	id, err := b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindStandalone,
		Level:        shadowdecisions.LevelWarning,
		Score:        3.2,
		Confidence:   0.8,
		ShadowOnly:   false, // will be rewritten
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id != 1 || len(w.rows) != 1 {
		t.Fatalf("expected 1 row; got %d (id=%d)", len(w.rows), id)
	}
	row := w.rows[0]
	if !row.ShadowOnly {
		t.Fatalf("bus must force ShadowOnly=true when promotion not allowed")
	}
	if row.StrategyVersion != "v11.5-test" {
		t.Fatalf("StrategyVersion not auto-stamped: %q", row.StrategyVersion)
	}
	if row.FiredAt.IsZero() {
		t.Fatalf("FiredAt must default to clock()")
	}
}

func TestRecord_DisabledStrategySkipped(t *testing.T) {
	w := &captureWriter{}
	b := New(newCfg(), w, nil, nil)
	id, err := b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "catalystwindow",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindBoost,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id != 0 || len(w.rows) != 0 {
		t.Fatalf("disabled strategy must not write rows")
	}
}

func TestRecord_UnknownStrategySkipped(t *testing.T) {
	w := &captureWriter{}
	b := New(newCfg(), w, nil, nil)
	id, _ := b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "phantom",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindTag,
	})
	if id != 0 || len(w.rows) != 0 {
		t.Fatalf("unknown strategy must not write rows")
	}
}

func TestRecord_PromotionGateForcesShadowEvenIfFlagSaysLive(t *testing.T) {
	cfg := newCfg()
	// conflictresolve is configured ShadowOnly=false; promotion is OFF.
	w := &captureWriter{}
	b := New(cfg, w, nil, nil)
	_, _ = b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "conflictresolve",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindSuppress,
		ShadowOnly:   false,
	})
	if !w.rows[0].ShadowOnly {
		t.Fatalf("promotion OFF must force ShadowOnly=true")
	}
}

func TestRecord_LivePathOnlyWhenPromotionAllowedAndFlagAllows(t *testing.T) {
	cfg := newCfg()
	cfg.GlobalPromotionAllowed = true
	w := &captureWriter{}
	b := New(cfg, w, nil, nil)
	_, _ = b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "conflictresolve",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindSuppress,
		ShadowOnly:   false,
	})
	if w.rows[0].ShadowOnly {
		t.Fatalf("live path: should keep ShadowOnly=false when both flags allow")
	}
	// thesisaccum stays shadow because per-strategy ShadowOnly=true.
	_, _ = b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-B",
		Kind:         shadowdecisions.KindStandalone,
		ShadowOnly:   false,
	})
	if !w.rows[1].ShadowOnly {
		t.Fatalf("per-strategy ShadowOnly=true must still force shadow even when global allows")
	}
}

func TestRecord_WriterErrorIsSurfacedNotPanic(t *testing.T) {
	w := &captureWriter{failNext: true}
	b := New(newCfg(), w, nil, nil)
	id, err := b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindStandalone,
	})
	if err == nil {
		t.Fatalf("expected error to surface")
	}
	if id != 0 {
		t.Fatalf("expected id=0 on writer error; got %d", id)
	}
}

func TestShouldEvaluate_RespectsFlag(t *testing.T) {
	b := New(newCfg(), nil, nil, nil)
	if !b.ShouldEvaluate("thesisaccum") {
		t.Fatalf("thesisaccum should be enabled")
	}
	if b.ShouldEvaluate("catalystwindow") {
		t.Fatalf("catalystwindow should be disabled")
	}
	if b.ShouldEvaluate("phantom") {
		t.Fatalf("unknown strategy must report disabled")
	}
}

func TestRecord_FiredAtDefaultsToClock(t *testing.T) {
	fixed := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	w := &captureWriter{}
	b := New(newCfg(), w, nil, nil).WithClock(func() time.Time { return fixed })
	_, _ = b.Record(context.Background(), shadowdecisions.Decision{
		StrategyName: "thesisaccum",
		ConditionID:  "cond-A",
		Kind:         shadowdecisions.KindStandalone,
	})
	if !w.rows[0].FiredAt.Equal(fixed) {
		t.Fatalf("FiredAt should equal clock(); got %v", w.rows[0].FiredAt)
	}
}
