// shadow_decisions_smoke_test.go — Part E smoke test for the v11.10
// strategy shadow decisions repository.
//
// Opt-in: set POSTGRES_TEST_DSN to a writable database that has the
// watchtower migrations applied. The test skips cleanly when DSN is
// absent so `go test ./...` stays hermetic.
//
// Asserts the round-trip: Record(Decision{shadow_only=true}) →
// the row lands in polymarket_strategy_shadow_decisions with the
// shadow_only flag preserved, reasons_json + features_json
// parseable, and a non-zero id.
package repository

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
)

// shadowSmokePool opens the test pool WITHOUT calling resetTables.
// The smoke test inserts one self-cleaning row marked with a
// "smoke" strategy_version so it is safe to run against a populated
// production database. The broader repository_test.go suite uses
// testPool() which TRUNCATEs — never call that from a smoke test.
func shadowSmokePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set; skipping shadow-decisions smoke test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestShadowDecisionsRepository_SmokeRoundTrip(t *testing.T) {
	pool := shadowSmokePool(t)
	ctx := context.Background()

	repo := NewShadowDecisionsRepository(pool)

	d := shadowdecisions.Decision{
		StrategyName:    "rulesrisk",
		StrategyVersion: "v11.10-insider-prior-smoke",
		ConditionID:     "0xsmoke000000000000000000000000000000000000000000000000000000000",
		EventSlug:       "smoke-test-event",
		Wallet:          "0xwalletsmoketestsmoketestsmoketestsmoketest",
		Side:            "YES",
		Kind:            shadowdecisions.KindTag,
		Level:           shadowdecisions.LevelInfo,
		Score:           0.42,
		Confidence:      0.55,
		Reasons:         []string{"smoke", "procedural_complexity_medium"},
		Features: map[string]any{
			"ambiguity_score": 0.42,
			"procedural_hits": 2,
		},
		ShadowOnly: true,
	}

	id, err := repo.Record(ctx, d)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected non-zero id; got %d", id)
	}

	// Verify the row via raw SQL — minimal coupling to sqlc-generated
	// types so the assertion is robust against query renames.
	var (
		shadowOnly      bool
		strategyName    string
		strategyVersion string
		reasonsJSON     []byte
		featuresJSON    []byte
		firedAt         time.Time
	)
	err = pool.QueryRow(ctx, `
SELECT shadow_only, strategy_name, strategy_version, reasons_json, features_json, fired_at
FROM polymarket_strategy_shadow_decisions
WHERE id = $1
`, id).Scan(&shadowOnly, &strategyName, &strategyVersion, &reasonsJSON, &featuresJSON, &firedAt)
	if err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if !shadowOnly {
		t.Fatalf("shadow_only must be preserved as true; got false")
	}
	if strategyName != "rulesrisk" {
		t.Fatalf("strategy_name mismatch: got %q", strategyName)
	}
	if strategyVersion != "v11.10-insider-prior-smoke" {
		t.Fatalf("strategy_version mismatch: got %q", strategyVersion)
	}
	var reasonsParsed []string
	if err := json.Unmarshal(reasonsJSON, &reasonsParsed); err != nil {
		t.Fatalf("reasons_json: %v", err)
	}
	if len(reasonsParsed) != 2 {
		t.Fatalf("reasons round-trip: got %v", reasonsParsed)
	}
	var featuresParsed map[string]any
	if err := json.Unmarshal(featuresJSON, &featuresParsed); err != nil {
		t.Fatalf("features_json: %v", err)
	}
	if _, ok := featuresParsed["ambiguity_score"]; !ok {
		t.Fatalf("features round-trip missing ambiguity_score; got %v", featuresParsed)
	}
	if firedAt.IsZero() {
		t.Fatalf("fired_at must be auto-populated by Record path")
	}

	// Cleanup — leave the table tidy for parallel smoke runs.
	if _, err := pool.Exec(ctx, `DELETE FROM polymarket_strategy_shadow_decisions WHERE id = $1`, id); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestShadowDecisionsRepository_PromotedRowPreservedAsLive proves the
// shadow_only=false path also survives the round-trip without flipping
// (the bus is responsible for setting the flag; the repo must not
// mutate it).
func TestShadowDecisionsRepository_PromotedRowPreservedAsLive(t *testing.T) {
	pool := shadowSmokePool(t)
	ctx := context.Background()
	repo := NewShadowDecisionsRepository(pool)

	d := shadowdecisions.Decision{
		StrategyName:    "thesisaccum",
		StrategyVersion: "v11.10-insider-prior-smoke-promoted",
		ConditionID:     "0xsmokepromo0000000000000000000000000000000000000000000000000000",
		Wallet:          "0xwalletsmokepromopromopromopromopromopromopromo",
		Side:            "YES",
		Kind:            shadowdecisions.KindStandalone,
		Level:           shadowdecisions.LevelWarning,
		Score:           2.5,
		Confidence:      0.85,
		Reasons:         []string{"promoted-smoke"},
		Features:        map[string]any{"breadth": 3, "consistency": 0.9},
		ShadowOnly:      false,
	}
	id, err := repo.Record(ctx, d)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM polymarket_strategy_shadow_decisions WHERE id = $1`, id)
	}()

	var so bool
	if err := pool.QueryRow(ctx, `SELECT shadow_only FROM polymarket_strategy_shadow_decisions WHERE id = $1`, id).Scan(&so); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if so {
		t.Fatalf("shadow_only must round-trip as false; got true")
	}
}
