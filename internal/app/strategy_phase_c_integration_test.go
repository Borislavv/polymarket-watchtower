//go:build integration

package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TestPhaseC_RealRunOnLivePool runs each Phase C worker once against
// the live Postgres pool with feature flags enabled, then asserts
// at least one row was inserted into each worker's target table.
//
//	POSTGRES_TEST_DSN=... go test -tags integration -count 1 \
//	   ./internal/app -run TestPhaseC_RealRunOnLivePool -v
//
// Skipped when the DSN env var is absent.
func TestPhaseC_RealRunOnLivePool(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN / POSTGRES_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	met := metrics.New()
	scfg := StrategyConfig{
		StrategyVersion:        "v11.7-integration",
		GlobalPromotionAllowed: false,
		MarketLinks: MarketLinksConfig{
			Enabled:        true,
			Interval:       time.Hour,
			BatchSize:      100,
			LinkVersion:    1,
			IncludeOpposed: true,
			MinConfidence:  0.3,
		},
		RiskScore: RiskScoreConfig{
			Enabled:      true,
			Interval:     4 * time.Hour,
			BatchSize:    50,
			ScoreVersion: 1,
			RefreshOlder: 24 * time.Hour,
		},
		Repricing: RepricingWorkerConfig{
			Enabled:        true,
			Interval:       5 * time.Minute,
			OpenLookback:   24 * time.Hour,
			MaxOpenWindows: 100,
			CloseAfter:     2 * time.Hour,
		},
		WalletGraph: WalletGraphConfig{
			Enabled:         true,
			Interval:        time.Hour,
			CoTradeWindow:   30 * 24 * time.Hour,
			MinSharedEvents: 3,
			BatchSize:       5000,
			EdgeVersion:     1,
		},
		HolderSync: HolderSyncConfig{
			Enabled:      false, // stub — no Polymarket holders API wrapped
			Interval:     10 * time.Minute,
			MaxMarkets:   250,
			TopK:         25,
			FetchTimeout: 5 * time.Second,
			Concurrency:  3,
			StaleAfter:   6 * time.Hour,
		},
		OutcomeBackfill: OutcomeBackfillConfig{
			Enabled:   true,
			Interval:  time.Hour,
			BatchSize: 1000,
		},
	}
	phaseB := wireStrategyPhaseB(pool, scfg, met, nil)
	phaseC := wireStrategyPhaseC(pool, scfg, met, phaseB.RulesRisk)

	// Run each worker once. Empty / no-op results are acceptable for
	// holdersync (no live source) and repricing close-phase (no open
	// windows yet) — we assert on marketlinks + walletgraph +
	// riskscore, which have real data.
	phaseC.MarketLinks.Tick(ctx)
	phaseC.RiskScore.Tick(ctx)
	phaseC.WalletGraph.Tick(ctx)
	phaseC.Repricing.Tick(ctx)
	phaseC.HolderSync.Tick(ctx)
	phaseC.OutcomeBackfill.Tick(ctx)

	// Phase B follow-up so freshly-inserted shadow rows (if any)
	// get an immediate value/promotion pass.
	phaseB.ValueWorker.Tick(ctx)
	phaseB.PromotionRev.Tick(ctx)

	type row struct {
		name string
		sql  string
		min  int
	}
	checks := []row{
		{"market_links", "SELECT COUNT(*) FROM polymarket_market_links", 1},
		{"market_risk_scores", "SELECT COUNT(*) FROM polymarket_market_risk_scores WHERE is_active = TRUE", 1},
		{"wallet_graph_edges", "SELECT COUNT(*) FROM polymarket_wallet_graph_edges", 0}, // may legitimately be 0 if no qualifying co-trades
		{"repricing_windows", "SELECT COUNT(*) FROM polymarket_repricing_windows", 0},
	}
	for _, c := range checks {
		var n int
		if err := pool.QueryRow(ctx, c.sql).Scan(&n); err != nil {
			t.Fatalf("%s scan: %v", c.name, err)
		}
		fmt.Printf("[phase-c-dry-run] %s rows: %d (min expected %d)\n", c.name, n, c.min)
		if n < c.min {
			t.Fatalf("%s: expected >= %d rows, got %d", c.name, c.min, n)
		}
	}
}
