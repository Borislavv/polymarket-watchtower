// holder_bookbars_test.go — v11.12-insider-prior smoke tests for the
// two new staged readers introduced for holderdelta + bookvacuum.
//
// Opt-in via POSTGRES_TEST_DSN. Each test seeds its own probe rows
// (marked with a unique condition_id prefix), exercises the reader,
// then cleans up. No truncation, safe to run alongside production
// data.
package stagedinputs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func smokePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set; skipping stagedinputs smoke tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestHolderSnapshotPairForWallet_RoundTrip(t *testing.T) {
	pool := smokePool(t)
	r := New(pool, Config{Enabled: true, CacheEnabled: false, MaxRows: 50, QueryTimeout: 2 * time.Second})
	ctx := context.Background()

	cond := fmt.Sprintf("0xprobe-%d", time.Now().UnixNano())
	token := "tok-yes"
	wallet := "0xprobewallet"

	// Insert two snapshots: prev (3h ago) + current (10m ago).
	now := time.Now().UTC()
	for i, sn := range []struct {
		at     time.Time
		rank   int
		shares float64
		pctOI  float64
	}{
		{now.Add(-3 * time.Hour), 12, 2_000, 0.04},
		{now.Add(-10 * time.Minute), 2, 10_000, 0.18},
	} {
		_, err := pool.Exec(ctx, `
INSERT INTO polymarket_holder_snapshots
  (condition_id, outcome_token, snapshot_at, wallet, rank, shares, notional_usd, pct_oi, total_oi)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			cond, token, sn.at, wallet, sn.rank, sn.shares, sn.shares*0.5, sn.pctOI, 55_000)
		if err != nil {
			t.Fatalf("seed snapshot %d: %v", i, err)
		}
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM polymarket_holder_snapshots WHERE condition_id = $1`, cond)
	}()

	pair, ok, err := r.HolderSnapshotPairForWallet(ctx, cond, token, wallet)
	if err != nil {
		t.Fatalf("reader err: %v", err)
	}
	if !ok {
		t.Fatalf("expected rows; got none")
	}
	if !pair.PreviousValid {
		t.Fatalf("PreviousValid must be true; got pair=%+v", pair)
	}
	if pair.Current.Rank != 2 || pair.Previous.Rank != 12 {
		t.Fatalf("snapshot order wrong: current=%+v previous=%+v", pair.Current, pair.Previous)
	}
	if pair.Current.PctOI != 0.18 {
		t.Fatalf("current pctOI: got %v want 0.18", pair.Current.PctOI)
	}
}

func TestHolderSnapshotPairForWallet_MissingReturnsFalse(t *testing.T) {
	pool := smokePool(t)
	r := New(pool, Config{Enabled: true, CacheEnabled: false, MaxRows: 50, QueryTimeout: 2 * time.Second})
	pair, ok, err := r.HolderSnapshotPairForWallet(context.Background(),
		"0xneverexisted", "tok", "0xwallet-never")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatalf("missing pair must return ok=false; got %+v", pair)
	}
}

func TestRecentBookFeatureBars_RoundTrip(t *testing.T) {
	pool := smokePool(t)
	r := New(pool, Config{Enabled: true, CacheEnabled: false, MaxRows: 50, QueryTimeout: 2 * time.Second})
	ctx := context.Background()

	cond := fmt.Sprintf("0xbookprobe-%d", time.Now().UnixNano())
	token := "tok-yes"
	now := time.Now().UTC()
	for i, sec := range []int{30, 90, 150} {
		_, err := pool.Exec(ctx, `
INSERT INTO polymarket_book_feature_bars
  (condition_id, outcome_token, bar_seconds, bar_start,
   best_bid, best_ask, mid_price,
   bid_depth_top_n, ask_depth_top_n,
   spread, spread_z, bid_depth_delta_pct, ask_depth_delta_pct, mid_delta)
VALUES ($1, $2, 15, $3, 0.49, 0.51, 0.50, 1000, 1000, 0.02, 0.5, 0, 0, 0.005)`,
			cond, token, now.Add(-time.Duration(sec)*time.Second))
		if err != nil {
			t.Fatalf("seed bar %d: %v", i, err)
		}
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM polymarket_book_feature_bars WHERE condition_id = $1`, cond)
	}()

	bars, err := r.RecentBookFeatureBars(ctx, cond, token, now.Add(-5*time.Minute), 0)
	if err != nil {
		t.Fatalf("reader err: %v", err)
	}
	if len(bars) != 3 {
		t.Fatalf("expected 3 bars; got %d", len(bars))
	}
	// Freshest-first
	if !bars[0].BarStart.After(bars[1].BarStart) {
		t.Fatalf("bars must be sorted freshest-first; got %v then %v", bars[0].BarStart, bars[1].BarStart)
	}
	if !bars[0].BidDepthValid || !bars[0].AskDepthValid {
		t.Fatalf("depth_valid flags must be true when depth is present")
	}
}

func TestRecentBookFeatureBars_RespectsSinceCutoff(t *testing.T) {
	pool := smokePool(t)
	r := New(pool, Config{Enabled: true, CacheEnabled: false, MaxRows: 50, QueryTimeout: 2 * time.Second})
	ctx := context.Background()

	cond := fmt.Sprintf("0xbookprobe-cut-%d", time.Now().UnixNano())
	token := "tok-no"
	now := time.Now().UTC()
	// Insert a 30s-old bar (in window) + an 1h-old bar (outside).
	for _, sec := range []int{30, 3600} {
		_, _ = pool.Exec(ctx, `
INSERT INTO polymarket_book_feature_bars
  (condition_id, outcome_token, bar_seconds, bar_start,
   best_bid, best_ask, mid_price, bid_depth_top_n, ask_depth_top_n, spread, spread_z,
   bid_depth_delta_pct, ask_depth_delta_pct, mid_delta)
VALUES ($1, $2, 15, $3, 0.49, 0.51, 0.50, 100, 100, 0.02, 0.1, 0, 0, 0.005)`,
			cond, token, now.Add(-time.Duration(sec)*time.Second))
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM polymarket_book_feature_bars WHERE condition_id = $1`, cond)
	}()

	bars, err := r.RecentBookFeatureBars(ctx, cond, token, now.Add(-5*time.Minute), 0)
	if err != nil {
		t.Fatalf("reader err: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("since-cutoff must exclude older bar; got %d", len(bars))
	}
}
