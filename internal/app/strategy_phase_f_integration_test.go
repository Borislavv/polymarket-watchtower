//go:build integration

package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/dataapi"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/httpx"
)

// TestPhaseF_RealHoldersAndBookbarsOnLivePool exercises the v11.10
// real-source path end-to-end against the live Polymarket public
// APIs + the local Postgres pool:
//
//  1. Constructs the dataapi client + the bookbars worker.
//  2. Runs holdersync once via the real `/holders` adapter.
//  3. Runs bookbars once via the real CLOB `/books` adapter.
//  4. Reads back the persisted rows and asserts non-zero.
//
// Requires:
//
//	POSTGRES_TEST_DSN=postgres://...
//	POLYMARKET_LIVE_SMOKE=1
//
// Skipped when either env var is absent.
func TestPhaseF_RealHoldersAndBookbarsOnLivePool(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN / POSTGRES_DSN unset")
	}
	if os.Getenv("POLYMARKET_LIVE_SMOKE") != "1" {
		t.Skip("POLYMARKET_LIVE_SMOKE=1 required for live API smoke")
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
		StrategyVersion: "v11.10-phase-f",
		HolderSync: HolderSyncConfig{
			Enabled:             true,
			WorkerEnabled:       true,
			SourceMode:          "dataapi",
			IntervalV2:          10 * time.Minute,
			MaxMarketsV2:        5,
			TopKV2:              25,
			PerMarketTimeout:    8 * time.Second,
			Concurrency:         2,
			StaleAfter:          6 * time.Hour,
			RequireOpenInterest: true,
		},
		BookFeatureBars: BookFeatureBarsConfig{
			Enabled:    true,
			Interval:   5 * time.Second,
			TopN:       5,
			MaxMarkets: 5,
		},
	}

	dataLog := zerolog.Nop()
	dataHTTP, err := httpx.New(httpx.Config{
		BaseURL:   "https://data-api.polymarket.com",
		Timeout:   10 * time.Second,
		UserAgent: "watchtower-phase-f-test",
		Logger:    &dataLog,
	})
	if err != nil {
		t.Fatalf("httpx: %v", err)
	}
	dataClient := dataapi.New(dataHTTP)

	phaseF, realHolder := wireStrategyPhaseF(pool, scfg, met, dataClient)
	if phaseF.BookBars == nil {
		t.Fatalf("bookbars worker must be wired")
	}
	if realHolder == nil {
		t.Fatalf("real holdersync worker must be wired when SourceMode=dataapi")
	}

	// Baseline row counts.
	var holdersBefore, bookbarsBefore int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM polymarket_holder_snapshots").Scan(&holdersBefore)
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM polymarket_book_feature_bars").Scan(&bookbarsBefore)

	// Tick each worker once.
	realHolder.Tick(ctx)
	phaseF.BookBars.Tick(ctx)

	var holdersAfter, bookbarsAfter int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM polymarket_holder_snapshots").Scan(&holdersAfter); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM polymarket_book_feature_bars").Scan(&bookbarsAfter); err != nil {
		t.Fatalf("count bookbars: %v", err)
	}
	fmt.Printf("[phase-f] holder_snapshots %d → %d\n", holdersBefore, holdersAfter)
	fmt.Printf("[phase-f] book_feature_bars %d → %d\n", bookbarsBefore, bookbarsAfter)

	if holdersAfter <= holdersBefore {
		t.Fatalf("expected new holder_snapshots after live tick (delta=0)")
	}
	if bookbarsAfter <= bookbarsBefore {
		t.Fatalf("expected new book_feature_bars after live tick (delta=0)")
	}

	// Sample row + show derived OI / depth.
	row := pool.QueryRow(ctx, `
SELECT condition_id, wallet, rank, shares, pct_oi, total_oi
FROM polymarket_holder_snapshots ORDER BY snapshot_at DESC LIMIT 1`)
	var (
		cid, wallet        string
		rank               int
		shares, pct, total float64
	)
	if err := row.Scan(&cid, &wallet, &rank, &shares, &pct, &total); err == nil {
		fmt.Printf("[phase-f] sample holder: cond=%s wallet=%s rank=%d shares=%.2f pct_oi=%.4f total_oi=%.2f\n",
			cid, wallet, rank, shares, pct, total)
	}
	row = pool.QueryRow(ctx, `
SELECT condition_id, outcome_token, best_bid, best_ask, mid_price, bid_depth_top_n, ask_depth_top_n
FROM polymarket_book_feature_bars ORDER BY bar_start DESC LIMIT 1`)
	var (
		cid2, tok           string
		bb, ba, mid, bd, ad float64
	)
	if err := row.Scan(&cid2, &tok, &bb, &ba, &mid, &bd, &ad); err == nil {
		fmt.Printf("[phase-f] sample bar: cond=%s tok=%s bb=%.4f ba=%.4f mid=%.4f bidD=%.1f askD=%.1f\n",
			cid2, tok, bb, ba, mid, bd, ad)
	}
}
