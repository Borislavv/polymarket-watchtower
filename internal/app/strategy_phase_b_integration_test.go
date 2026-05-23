//go:build integration

package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TestPhaseB_RealShadowRowFromLivePool is the v11.6 PART 9 dry-run
// proof. Connects to POSTGRES_TEST_DSN (or POSTGRES_DSN), wires the
// real Phase B bundle, writes ONE shadow row via the bus, runs the
// value evaluator and promotion worker once, then asserts the row
// is visible.
//
//	go test -tags integration -count 1 ./internal/app -run TestPhaseB_RealShadowRowFromLivePool
//
// Skipped when the DSN env var is absent so CI without Postgres
// stays green.
func TestPhaseB_RealShadowRowFromLivePool(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN / POSTGRES_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	met := metrics.New()
	scfg := StrategyConfig{
		StrategyVersion:        "v11.6-integration",
		GlobalPromotionAllowed: false,
		ThesisAccum:            ThesisAccumConfig{Enabled: true, ShadowOnly: true},
	}
	bundle := wireStrategyPhaseB(pool, scfg, met, nil)
	if bundle.Bus == nil {
		t.Fatalf("bus must be wired against live pool")
	}

	probe := shadowdecisions.Decision{
		StrategyName:    "thesisaccum",
		StrategyVersion: "v11.6-integration",
		ConditionID:     "0xINTEGRATION_PROBE",
		EventSlug:       "phase-b-integration",
		Wallet:          "0xprobe",
		Side:            "BUY",
		Kind:            shadowdecisions.KindStandalone,
		Level:           shadowdecisions.LevelInfo,
		Score:           1.23,
		Confidence:      0.42,
		Reasons:         []string{"phase_b_integration_probe"},
		Features:        map[string]any{"probe": true},
		ShadowOnly:      true,
		FiredAt:         time.Now(),
	}
	id, err := bundle.Bus.Record(ctx, probe)
	if err != nil {
		t.Fatalf("bus.Record: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected real row id; got 0")
	}

	// Run the value evaluator + promotion worker once.
	bundle.ValueWorker.Tick(ctx)
	bundle.PromotionRev.Tick(ctx)

	// Assert the row exists.
	row := pool.QueryRow(ctx,
		`SELECT id, strategy_name, condition_id, shadow_only
		 FROM polymarket_strategy_shadow_decisions WHERE id = $1`, id)
	var (
		gotID     int64
		gotName   string
		gotCondID string
		gotShadow bool
	)
	if err := row.Scan(&gotID, &gotName, &gotCondID, &gotShadow); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotName != "thesisaccum" {
		t.Fatalf("strategy_name mismatch: %q", gotName)
	}
	if !gotShadow {
		t.Fatalf("bus must have forced shadow_only=true (promotion not allowed)")
	}

	// Clean up the probe row.
	if _, err := pool.Exec(ctx, `DELETE FROM polymarket_strategy_shadow_decisions WHERE id = $1`, id); err != nil {
		t.Logf("cleanup failed (non-fatal): %v", err)
	}
}
