// Repository integration tests. Opt-in: set POSTGRES_TEST_DSN to a writable
// database that has the watchtower migrations applied. The simplest way to
// run them locally:
//
//	docker compose -f deploy/docker-compose.yml up -d postgres
//	go run ./cmd/cli migrate -dsn "postgres://watchtower:watchtower@localhost:5433/watchtower?sslmode=disable"
//	POSTGRES_TEST_DSN="postgres://watchtower:watchtower@localhost:5433/watchtower?sslmode=disable" go test ./internal/infra/repository/...
//
// Tests truncate every table in their own setup so they're order-
// independent. They MUST NOT run against a production database.
package repository

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set; skipping repository integration tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	return pool
}

func resetTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// TRUNCATE … CASCADE wipes everything in one statement and respects
	// FKs without needing per-table DELETE ordering.
	if _, err := pool.Exec(context.Background(), `
        TRUNCATE TABLE
            polymarket_alerts,
            polymarket_trades,
            polymarket_market_outcomes,
            polymarket_market_categories,
            polymarket_markets,
            polymarket_traders,
            polymarket_categories
        RESTART IDENTITY CASCADE
    `); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func TestCategoryRepository_UpsertAndWhitelist(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := NewCategoryRepository(pool)

	seen, err := repo.UpsertSeen(ctx, []Category{
		{ExternalID: "100", Slug: "politics", Name: "Politics"},
		{ExternalID: "200", Slug: "sports", Name: "Sports"},
		{ExternalID: "300", Slug: "macro", Name: "Macro"},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("upsert returned %d rows want 3", len(seen))
	}

	// Re-upsert is idempotent (same external IDs, possibly mutated name).
	if _, err := repo.UpsertSeen(ctx, []Category{
		{ExternalID: "100", Slug: "politics", Name: "Politics & Policy"},
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	enabled, err := repo.ApplyWhitelist(ctx, []string{"Politics", "Macro"})
	if err != nil {
		t.Fatalf("apply whitelist: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled, got %d: %+v", len(enabled), enabled)
	}
	listed, err := repo.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("ListEnabled returned %d want 2", len(listed))
	}

	// Re-applying with the empty whitelist disables everything we'd
	// previously enabled.
	if _, err := repo.ApplyWhitelist(ctx, nil); err != nil {
		t.Fatalf("apply empty whitelist: %v", err)
	}
	listed, _ = repo.ListEnabled(ctx)
	if len(listed) != 0 {
		t.Errorf("expected nothing enabled after empty whitelist, got %d", len(listed))
	}
}

func TestMarketRepository_UpsertAndBackfillState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	catRepo := NewCategoryRepository(pool)
	cats, err := catRepo.UpsertSeen(ctx, []Category{
		{ExternalID: "100", Slug: "politics", Name: "Politics"},
	})
	if err != nil {
		t.Fatalf("seed categories: %v", err)
	}
	politicsID := cats[0].ID

	marketRepo := NewMarketRepository(pool)
	now := time.Now().UTC().Truncate(time.Second)
	upserted, err := marketRepo.UpsertSeen(ctx, []UpsertMarketInput{
		{
			ConditionID: "0xCAFE",
			Slug:        "us-election-2028",
			Question:    "Will X win?",
			EventSlug:   "us-election-2028",
			EventTitle:  "US Election 2028",
			StartDate:   now.Add(-30 * 24 * time.Hour),
			EndDate:     now.Add(60 * 24 * time.Hour),
			CategoryIDs: []int64{politicsID},
		},
	})
	if err != nil {
		t.Fatalf("upsert market: %v", err)
	}
	if len(upserted) != 1 || upserted[0].ConditionID != "0xCAFE" {
		t.Fatalf("upserted: %+v", upserted)
	}
	if upserted[0].BackfillStatus != BackfillPending {
		t.Errorf("new market default status: got %q want pending", upserted[0].BackfillStatus)
	}

	// Backfill state transitions: begin → complete.
	if err := marketRepo.BeginBackfill(ctx, upserted[0].ID); err != nil {
		t.Fatalf("begin backfill: %v", err)
	}
	if err := marketRepo.CompleteBackfill(ctx, upserted[0].ID, BackfillCompleted, now.Add(-24*time.Hour), now); err != nil {
		t.Fatalf("complete backfill: %v", err)
	}
	got, err := marketRepo.GetByConditionID(ctx, "0xCAFE")
	if err != nil {
		t.Fatalf("get market: %v", err)
	}
	if got.BackfillStatus != BackfillCompleted {
		t.Errorf("status after complete: %q", got.BackfillStatus)
	}

	// Mark-inactive: a market not present in the latest sweep within the
	// whitelisted scope becomes inactive.
	if err := marketRepo.MarkSeenInactive(ctx, []string{"someone-else"}, []int64{politicsID}); err != nil {
		t.Fatalf("mark inactive: %v", err)
	}
	got, _ = marketRepo.GetByConditionID(ctx, "0xCAFE")
	if got.Active {
		t.Error("market should be inactive after missing from sweep")
	}

	// ListActiveForCollection respects the active flag.
	listed, err := marketRepo.ListActiveForCollection(ctx)
	if err != nil {
		t.Fatalf("list active for collection: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("inactive markets must not appear in collection list")
	}
}

func TestTradeRepository_UpsertIdempotentAndBaseline(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Minimal seed: one market under one category.
	cats, _ := NewCategoryRepository(pool).UpsertSeen(ctx, []Category{
		{ExternalID: "100", Slug: "politics", Name: "Politics"},
	})
	mkts, err := NewMarketRepository(pool).UpsertSeen(ctx, []UpsertMarketInput{
		{
			ConditionID: "0xCAFE", Slug: "m", Question: "q",
			StartDate:   time.Now().UTC().Add(-30 * 24 * time.Hour),
			EndDate:     time.Now().UTC().Add(60 * 24 * time.Hour),
			CategoryIDs: []int64{cats[0].ID},
		},
	})
	if err != nil {
		t.Fatalf("seed market: %v", err)
	}
	marketID := mkts[0].ID

	repo := NewTradeRepository(pool)
	traderRepo := NewTraderRepository(pool)
	traders, _ := traderRepo.UpsertSeen(ctx, []string{"0xWHALE"})
	traderID := traders[0].ID

	now := time.Now().UTC().Truncate(time.Second)
	trades := make([]InsertTradeInput, 0, 5)
	for i := 0; i < 5; i++ {
		trades = append(trades, InsertTradeInput{
			MarketID:     marketID,
			TraderID:     &traderID,
			OutcomeToken: "tok-yes",
			Side:         "BUY",
			Price:        0.5,
			SizeShares:   100,
			NotionalUSD:  50,
			TradedAt:     now.Add(time.Duration(-i) * time.Hour),
			ExternalID:   "ext-" + string(rune('A'+i)),
		})
	}
	res, err := repo.UpsertBatch(ctx, trades)
	if err != nil {
		t.Fatalf("upsert trades: %v", err)
	}
	if res.Inserted != 5 {
		t.Errorf("inserted: %d want 5", res.Inserted)
	}

	// Replay: same dedup keys → no new inserts.
	res2, _ := repo.UpsertBatch(ctx, trades)
	if res2.Inserted != 0 {
		t.Errorf("re-upsert inserted %d want 0", res2.Inserted)
	}

	// Baseline read.
	since := now.Add(-12 * time.Hour)
	got, err := repo.ListBaseline(ctx, BaselineQuery{
		MarketID: marketID, OutcomeToken: "tok-yes", Since: since,
	})
	if err != nil {
		t.Fatalf("list baseline: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("baseline count: %d want 5", len(got))
	}

	// Summary span equals oldest..newest of the seeded trades.
	summary, err := repo.SummarizeBaseline(ctx, BaselineQuery{
		MarketID: marketID, OutcomeToken: "tok-yes", Since: since,
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if summary.SampleCount != 5 {
		t.Errorf("summary count: %d want 5", summary.SampleCount)
	}
	if summary.Span() < 3*time.Hour || summary.Span() > 5*time.Hour {
		t.Errorf("span: %s want ~4h", summary.Span())
	}
}

func TestAlertRepository_DedupAndFlow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := NewAlertRepository(pool)

	payload, _ := json.Marshal(map[string]any{"sev": "info", "mul": 270})
	a := NewAlert{
		DedupKey:        "single:v1:test-key-1",
		StrategyVersion: "v1",
		Kind:            AlertKindTrade,
		Reason:          "LargeRareBet",
		Severity:        "info",
		Payload:         payload,
	}

	created, ok, err := repo.TryCreatePending(ctx, a)
	if err != nil {
		t.Fatalf("try create: %v", err)
	}
	if !ok || created.ID == 0 {
		t.Fatalf("expected fresh insert, got created=%v alert=%+v", ok, created)
	}

	// Re-insert with same dedup_key → no-op.
	_, ok2, err := repo.TryCreatePending(ctx, a)
	if err != nil {
		t.Fatalf("dup try create: %v", err)
	}
	if ok2 {
		t.Fatal("second TryCreatePending must return created=false")
	}

	// Claim → mark sent.
	claims, err := repo.ClaimPending(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claims) != 1 || claims[0].ID != created.ID {
		t.Fatalf("claims: %+v", claims)
	}
	if err := repo.MarkSent(ctx, created.ID, 42); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	// After MarkSent the row is no longer pending.
	leftover, _ := repo.ClaimPending(ctx, 10)
	if len(leftover) != 0 {
		t.Errorf("expected 0 pending after mark sent, got %d", len(leftover))
	}
}
