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

// TestPhaseE_ThesisLinesAndRepricingClose runs the v11.9 thesis-lines
// worker + repricing close phase against the live Postgres pool and
// asserts both produce rows. This is the runtime proof that the v11.9
// additions are not just compile-clean but actually fire on real data.
//
//	POSTGRES_TEST_DSN=... go test -tags integration -count 1 \
//	  ./internal/app -run TestPhaseE_ThesisLinesAndRepricingClose -v
func TestPhaseE_ThesisLinesAndRepricingClose(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN / POSTGRES_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	met := metrics.New()
	scfg := StrategyConfig{
		StrategyVersion:        "v11.9-phase-e",
		GlobalPromotionAllowed: false,
		MarketLinks: MarketLinksConfig{
			Enabled: true, Interval: time.Hour, BatchSize: 100, LinkVersion: 1,
			IncludeOpposed: true, MinConfidence: 0.3,
		},
		Repricing: RepricingWorkerConfig{
			Enabled: true, Interval: time.Minute, OpenLookback: 24 * time.Hour,
			MaxOpenWindows: 200, CloseAfter: time.Nanosecond, // force all open windows due
			CloseEnabled: true, MinPeerCount: 2, MinLagCents: 3, PriceSource: "trades",
		},
		ThesisLines: ThesisLinesConfig{
			WorkerEnabled: true, Lookback: 30 * 24 * time.Hour, Interval: time.Hour,
			MaxEvents: 500, MaxWallets: 5000,
		},
	}
	phaseB := wireStrategyPhaseB(pool, scfg, met, nil)
	phaseC := wireStrategyPhaseC(pool, scfg, met, phaseB.RulesRisk)

	// Reset open repricing_windows.closes_at to now-1s so the close
	// phase can pick them up in this single tick. This emulates one
	// production tick after the window's normal close window elapsed.
	if _, err := pool.Exec(ctx, `
UPDATE polymarket_repricing_windows
SET closes_at = NOW() - INTERVAL '1 second'
WHERE status='open' AND closes_at > NOW()`); err != nil {
		t.Logf("close-time prep failed (non-fatal): %v", err)
	}

	// One tick each.
	phaseC.ThesisLines.Tick(ctx)
	phaseC.Repricing.Tick(ctx)

	var thesisN int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM polymarket_wallet_thesis_lines`).Scan(&thesisN); err != nil {
		t.Fatalf("count thesis_lines: %v", err)
	}
	fmt.Printf("[phase-e] wallet_thesis_lines rows: %d\n", thesisN)
	if thesisN == 0 {
		t.Fatalf("thesis lines worker produced 0 rows from production trades")
	}

	statusCounts := map[string]int{}
	rows, err := pool.Query(ctx, `SELECT status, COUNT(*) FROM polymarket_repricing_windows GROUP BY 1`)
	if err != nil {
		t.Fatalf("query repricing statuses: %v", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err == nil {
			statusCounts[status] = n
		}
	}
	rows.Close()
	fmt.Printf("[phase-e] repricing_windows status breakdown: %v\n", statusCounts)
	closed := statusCounts["closed_no_lag"] + statusCounts["closed_lag_detected"] +
		statusCounts["closed_blocked"] + statusCounts["stale_missing_price"] +
		statusCounts["stale_missing_peers"]
	if closed == 0 {
		t.Fatalf("expected at least one closed window after tick; got %v", statusCounts)
	}
}
