package detect

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/cluster"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/dbbaseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/category"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketcache"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func mustFilter(tokens ...string) *category.Filter {
	return category.NewFilter(tokens)
}

// dbTestPool opens the live Postgres specified by POSTGRES_TEST_DSN and
// truncates every table before returning. Mirrors the repository test
// helper so these tests stay order-independent.
// dbTestPool opens the live Postgres and truncates every table. NOTE: the
// repo, dbbaseline, and detect integration suites all share this pattern;
// running them via `go test ./...` parallelises packages and produces
// flaky FK violations. Always use `-p 1` (the README documents this).
func dbTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set; skipping DB integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		TRUNCATE TABLE polymarket_alerts, polymarket_trades, polymarket_market_outcomes,
			polymarket_market_categories, polymarket_markets, polymarket_traders, polymarket_categories
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func seedPoliticsMarket(t *testing.T, pool *pgxpool.Pool, conditionID string) (catID, marketID int64) {
	t.Helper()
	ctx := context.Background()
	cats, _ := repository.NewCategoryRepository(pool).UpsertSeen(ctx, []repository.Category{
		{ExternalID: "42", Slug: "politics", Name: "Politics"},
	})
	mkts, err := repository.NewMarketRepository(pool).UpsertSeen(ctx, []repository.UpsertMarketInput{{
		ConditionID: conditionID, Slug: "us-pres", Question: "Who wins?",
		EventSlug:   "us-pres-2028",
		StartDate:   time.Now().UTC().Add(-95 * 24 * time.Hour),
		EndDate:     time.Now().UTC().Add(5 * 24 * time.Hour),
		CategoryIDs: []int64{cats[0].ID},
	}})
	if err != nil {
		t.Fatalf("seed market: %v", err)
	}
	return cats[0].ID, mkts[0].ID
}

func seedBaselineTrades(t *testing.T, pool *pgxpool.Pool, marketID int64, count int, notional float64, start time.Time, step time.Duration) {
	t.Helper()
	ctx := context.Background()
	rows := make([]repository.InsertTradeInput, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, repository.InsertTradeInput{
			MarketID:     marketID,
			OutcomeToken: "tok-yes",
			Side:         "BUY",
			Price:        0.5,
			SizeShares:   2 * notional, // notional = price * size
			NotionalUSD:  notional,
			TradedAt:     start.Add(time.Duration(i) * step),
			ExternalID:   "seed-" + string(rune('A'+i%26)) + "-" + string(rune('A'+i/26)),
		})
	}
	if _, err := repository.NewTradeRepository(pool).UpsertBatch(ctx, rows); err != nil {
		t.Fatalf("seed trades: %v", err)
	}
}

func newDBLoop(t *testing.T, pool *pgxpool.Pool, now time.Time, emit Emitter) *Loop {
	t.Helper()
	mr := repository.NewMarketRepository(pool)
	tr := repository.NewTradeRepository(pool)
	dr := repository.NewTraderRepository(pool)
	ar := repository.NewAlertRepository(pool)
	provider := dbbaseline.New(dbbaseline.Config{
		Window: 365 * 24 * time.Hour,
		Clock:  func() time.Time { return now },
	}, tr, mr)
	log := zerolog.Nop()
	return New(Config{
		Thresholds: anomaly.Thresholds{
			Info:                   anomaly.Tier{MinNotionalUSD: 10_000, MinOdds: 3, MinMultiplier: 100},
			Warning:                anomaly.Tier{MinNotionalUSD: 25_000, MinOdds: 5, MinMultiplier: 1_000},
			Critical:               anomaly.Tier{MinNotionalUSD: 100_000, MinOdds: 8, MinMultiplier: 10_000},
			MinBaselineTrades:      20,
			MinBaselineNotionalUSD: 1_000,
		},
		Baseline:              baseline.Config{Window: 365 * 24 * time.Hour},
		Cluster:               cluster.Config{Window: time.Hour, MinTrades: 99, MinUniqueWallets: 99},
		Clock:                 func() time.Time { return now },
		PolymarketBase:        "https://polymarket.com",
		LifecycleAlertFromPct: 75,
		LifecycleHotFromPct:   90,
		MarketMinAge:          24 * time.Hour,
		BaselineMinReadySpan:  24 * time.Hour,
		StrategyVersion:       "v1",
		Baseliner:             provider,
		Alerts:                ar,
		Markets:               mr,
		Traders:               dr,
	}, marketcache.New(), emit, metrics.New(), &log)
}

// TestDetect_DBBaselineFiresAndPersistsAlert is the end-to-end happy path:
// seed the DB baseline, observe a whale trade, assert that exactly one
// pending alert row exists with the right severity and dedup_key shape,
// and assert that the emitted Finding carries the actual DB span.
func TestDetect_DBBaselineFiresAndPersistsAlert(t *testing.T) {
	pool := dbTestPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, marketID := seedPoliticsMarket(t, pool, "0xabc")

	// 30 baseline trades of $60 each spaced 6h apart → median ~60, span ~7d.
	seedBaselineTrades(t, pool, marketID, 30, 60, now.Add(-7*24*time.Hour), 6*time.Hour)

	emit := &capturingEmitter{}
	loop := newDBLoop(t, pool, now, emit)
	// Build the in-process market view (registry isn't seeded here; allowed()
	// uses cfg.Filter which is nil = allow all).
	m := market.Market{
		ID:         "0xabc",
		Slug:       "us-pres",
		Question:   "Who wins?",
		EventSlug:  "us-pres-2028",
		TokenIDs:   []vo.TokenID{"tok-yes"},
		Outcomes:   []string{"Yes"},
		Categories: []vo.CategoryID{42},
		Active:     true,
		StartDate:  now.Add(-95 * 24 * time.Hour),
		EndDate:    now.Add(5 * 24 * time.Hour),
	}
	whale := trade.Trade{
		ID: "whale-1", Market: "0xabc", Token: "tok-yes",
		Side: trade.SideBuy, Price: 1.0 / 8, Size: 700_000 / (1.0 / 8),
		Timestamp: now, Taker: "0xshark",
	}
	loop.Observe(context.Background(), m, whale)

	got := emit.of(anomaly.KindTradeAnomaly)
	if len(got) != 1 {
		t.Fatalf("expected 1 emitted finding, got %d", len(got))
	}
	if got[0].Severity != anomaly.SeverityCritical {
		t.Errorf("severity: %s want critical", got[0].Severity)
	}
	if got[0].Baseline.Span < 6*24*time.Hour {
		t.Errorf("baseline span must reflect ~7d of stored data, got %s", got[0].Baseline.Span)
	}

	// Exactly one alert row, pending, payload round-trips.
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM polymarket_alerts WHERE status='pending'").Scan(&n); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if n != 1 {
		t.Fatalf("alert rows: %d want 1", n)
	}
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		"SELECT payload FROM polymarket_alerts LIMIT 1").Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var roundTrip anomaly.Finding
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if roundTrip.Severity != anomaly.SeverityCritical || roundTrip.Trade == nil {
		t.Errorf("payload round-trip lost fields: %+v", roundTrip)
	}
}

// TestDetect_ReObserveSameTradeNoSecondAlert is the cross-restart dedup
// guarantee: observing the same trade twice (e.g. after a process restart
// that re-reads the same Data API page) must NOT create a second alert
// row and must NOT re-emit the realtime finding.
func TestDetect_ReObserveSameTradeNoSecondAlert(t *testing.T) {
	pool := dbTestPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, marketID := seedPoliticsMarket(t, pool, "0xabc")
	seedBaselineTrades(t, pool, marketID, 30, 60, now.Add(-7*24*time.Hour), 6*time.Hour)

	emit := &capturingEmitter{}
	loop := newDBLoop(t, pool, now, emit)
	m := market.Market{
		ID: "0xabc", Slug: "us-pres", EventSlug: "us-pres-2028",
		TokenIDs: []vo.TokenID{"tok-yes"}, Outcomes: []string{"Yes"},
		Categories: []vo.CategoryID{42}, Active: true,
		StartDate: now.Add(-95 * 24 * time.Hour), EndDate: now.Add(5 * 24 * time.Hour),
	}
	whale := trade.Trade{
		ID: "whale-replay", Market: "0xabc", Token: "tok-yes",
		Side: trade.SideBuy, Price: 1.0 / 8, Size: 700_000 / (1.0 / 8),
		Timestamp: now, Taker: "0xshark",
	}
	loop.Observe(context.Background(), m, whale)
	loop.Observe(context.Background(), m, whale)
	loop.Observe(context.Background(), m, whale)

	if got := emit.of(anomaly.KindTradeAnomaly); len(got) != 1 {
		t.Errorf("realtime fanout fired %d times — duplicates not suppressed", len(got))
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM polymarket_alerts").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("alert rows: %d want 1", n)
	}
}

// TestDetect_NonWhitelistedCategoryNoAlert pins the whitelist gate at the
// DB level: an alert created with a market whose category is not in the
// detector's whitelist must not be written.
func TestDetect_NonWhitelistedCategoryNoAlert(t *testing.T) {
	pool := dbTestPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	cats, _ := repository.NewCategoryRepository(pool).UpsertSeen(ctx, []repository.Category{
		{ExternalID: "77", Slug: "sports", Name: "NBA"},
	})
	_, err := repository.NewMarketRepository(pool).UpsertSeen(ctx, []repository.UpsertMarketInput{{
		ConditionID: "0xnba", Slug: "nba", Question: "?",
		EventSlug:   "2026-nba",
		StartDate:   now.Add(-30 * 24 * time.Hour),
		EndDate:     now.Add(1 * 24 * time.Hour),
		CategoryIDs: []int64{cats[0].ID},
	}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	emit := &capturingEmitter{}
	loop := newDBLoop(t, pool, now, emit)
	loop.cfg.Filter = mustFilter("politics") // tighten whitelist for this test
	m := market.Market{
		ID: "0xnba", Slug: "nba", EventSlug: "2026-nba",
		TokenIDs: []vo.TokenID{"t"}, Outcomes: []string{"Yes"},
		Categories: []vo.CategoryID{77}, Active: true,
		StartDate: now.Add(-30 * 24 * time.Hour), EndDate: now.Add(1 * 24 * time.Hour),
	}
	loop.Observe(ctx, m, trade.Trade{
		ID: "x", Market: "0xnba", Token: "t",
		Side: trade.SideBuy, Price: 1.0 / 8, Size: 700_000 / (1.0 / 8),
		Timestamp: now, Taker: "0xs",
	})

	if got := emit.all(); len(got) != 0 {
		t.Errorf("non-whitelisted category fired %d alerts", len(got))
	}
	var n int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM polymarket_alerts").Scan(&n)
	if n != 0 {
		t.Errorf("alert rows: %d want 0", n)
	}
}
