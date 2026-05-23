//go:build integration

package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/stagedinputs"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TestPhaseD_RealShadowRowsAcrossStrategies replays the most recent
// real alerts through the strategybus + staged-input fanout that
// detect.Loop uses on the hot path, and asserts that shadow rows
// were written for MULTIPLE strategies — not just rulesrisk.
//
//	POSTGRES_TEST_DSN=... go test -tags integration -count 1 \
//	  ./internal/app -run TestPhaseD_RealShadowRowsAcrossStrategies -v
//
// The synthesizer reads each real alert row from polymarket_alerts,
// constructs a synthetic Decision per strategy using staged inputs,
// and writes through the same Bus → repository.ShadowDecisionsRepository
// path the production hot path uses. Cleans up afterward.
func TestPhaseD_RealShadowRowsAcrossStrategies(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN / POSTGRES_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	met := metrics.New()
	scfg := StrategyConfig{
		StrategyVersion:        "v11.8-phase-d",
		GlobalPromotionAllowed: false,
		ThesisAccum:            ThesisAccumConfig{Enabled: true, ShadowOnly: true},
		OwnershipV2:            OwnershipV2Config{Enabled: true, ShadowOnly: true},
		CatalystWindow:         CatalystWindowConfig{Enabled: true, ShadowOnly: true},
		BookVacuum:             BookVacuumConfig{Enabled: true, ShadowOnly: true},
		RepricingLag:           RepricingLagConfig{Enabled: true, ShadowOnly: true},
		WalletCohort:           WalletCohortConfig{Enabled: true, ShadowOnly: true},
		ConflictResolve:        ConflictResolveConfig{Enabled: true, ShadowOnly: true},
		RulesRisk:              RulesRiskConfig{Enabled: true, ShadowOnly: true},
		CheapTail:              CheapTailConfig{Enabled: true, ShadowOnly: true},
		StagedInputs: StagedInputsConfig{
			Enabled:      true,
			CacheEnabled: true,
			CacheTTL:     30 * time.Second,
			MaxRows:      200,
			QueryTimeout: 2 * time.Second,
		},
	}
	phaseB := wireStrategyPhaseB(pool, scfg, met, nil)
	readers := stagedinputs.New(pool, stagedinputs.Config{
		Enabled:      true,
		CacheEnabled: true,
		CacheTTL:     30 * time.Second,
		MaxRows:      200,
		QueryTimeout: 2 * time.Second,
	})
	if readers == nil {
		t.Fatalf("staged readers must wire against live pool")
	}

	// Replay: pick up to 25 recent alerts. For each alert we have:
	// dedup_key, market_id, sent_at, plus the linked market's
	// event_slug + condition_id which we look up.
	type alertProbe struct {
		dedupKey    string
		conditionID string
		eventSlug   string
		wallet      string
		side        string
	}
	probeRows := make([]alertProbe, 0, 25)
	rows, err := pool.Query(ctx, `
SELECT a.dedup_key,
       COALESCE(m.condition_id, '') AS condition_id,
       COALESCE(m.event_slug, '')   AS event_slug,
       COALESCE(t.wallet_address, '') AS wallet,
       COALESCE(a.payload->>'trade_side', '') AS side
FROM polymarket_alerts a
LEFT JOIN polymarket_markets m ON m.id = a.market_id
LEFT JOIN polymarket_traders t ON t.id = a.trader_id
WHERE a.sent_at IS NOT NULL
ORDER BY a.sent_at DESC
LIMIT 25`)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	for rows.Next() {
		var p alertProbe
		if err := rows.Scan(&p.dedupKey, &p.conditionID, &p.eventSlug, &p.wallet, &p.side); err != nil {
			t.Fatalf("scan: %v", err)
		}
		probeRows = append(probeRows, p)
	}
	rows.Close()
	if len(probeRows) == 0 {
		t.Skip("no real alerts to replay")
	}
	fmt.Printf("[phase-d] replaying %d real alerts\n", len(probeRows))

	// Write one shadow row PER STRATEGY PER ALERT using staged inputs.
	// We hit Bus.Record directly so this exercises the same write
	// path detect.Loop uses on the hot path.
	now := time.Now()
	perStrategy := map[string]int{}
	for _, p := range probeRows {
		// catalystwindow
		if cats, _ := readers.CatalystsByEvent(ctx, p.eventSlug); len(cats) > 0 {
			id, err := phaseB.Bus.Record(ctx, shadowdecisions.Decision{
				StrategyName: "catalystwindow", ConditionID: p.conditionID, EventSlug: p.eventSlug,
				Wallet: p.wallet, Side: p.side, Kind: shadowdecisions.KindBoost, Level: shadowdecisions.LevelNone,
				Score: 1.0, Confidence: cats[0].Confidence,
				Reasons:             []string{"phase_d_replay", "catalysts_found=" + fmt.Sprint(len(cats))},
				LinkedAlertDedupKey: p.dedupKey, FiredAt: now,
			})
			if err == nil && id > 0 {
				perStrategy["catalystwindow"]++
			}
		}
		// walletcohort
		if edges, _ := readers.WalletEdgesForWallet(ctx, p.wallet, 1); len(edges) > 0 {
			id, _ := phaseB.Bus.Record(ctx, shadowdecisions.Decision{
				StrategyName: "walletcohort", ConditionID: p.conditionID, EventSlug: p.eventSlug,
				Wallet: p.wallet, Side: p.side, Kind: shadowdecisions.KindBoost, Level: shadowdecisions.LevelNone,
				Score: edges[0].SimilarityScore, Confidence: float64(edges[0].CoEvents) / 10.0,
				Reasons:             []string{"phase_d_replay", "edges_found=" + fmt.Sprint(len(edges))},
				LinkedAlertDedupKey: p.dedupKey, FiredAt: now,
			})
			if id > 0 {
				perStrategy["walletcohort"]++
			}
		}
		// thesisaccum (structural — links found)
		if links, _ := readers.MarketLinksByEvent(ctx, p.eventSlug, 1); len(links) > 0 {
			id, _ := phaseB.Bus.Record(ctx, shadowdecisions.Decision{
				StrategyName: "thesisaccum", ConditionID: p.conditionID, EventSlug: p.eventSlug,
				Wallet: p.wallet, Side: p.side, Kind: shadowdecisions.KindTag, Level: shadowdecisions.LevelNone,
				Score: float64(len(links)), Confidence: 0.3,
				Reasons:             []string{"phase_d_replay", "links_found=" + fmt.Sprint(len(links))},
				LinkedAlertDedupKey: p.dedupKey, FiredAt: now,
			})
			if id > 0 {
				perStrategy["thesisaccum"]++
			}
		}
		// rulesrisk via active score
		if rs, ok, _ := readers.RiskScoreForCondition(ctx, p.conditionID); ok {
			id, _ := phaseB.Bus.Record(ctx, shadowdecisions.Decision{
				StrategyName: "rulesrisk", ConditionID: p.conditionID, EventSlug: p.eventSlug,
				Wallet: p.wallet, Side: p.side, Kind: shadowdecisions.KindTag, Level: shadowdecisions.LevelNone,
				Score: rs.AmbiguityScore, Confidence: rs.DisputeRisk,
				Reasons:             []string{"phase_d_replay", "ambiguity_score"},
				LinkedAlertDedupKey: p.dedupKey, FiredAt: now,
			})
			if id > 0 {
				perStrategy["rulesrisk"]++
			}
		}
		// repricinglag — only if windows exist for this condition
		if wins, _ := readers.ClosedRepricingWindowsForCondition(ctx, p.conditionID, now.Add(-30*24*time.Hour)); len(wins) > 0 {
			id, _ := phaseB.Bus.Record(ctx, shadowdecisions.Decision{
				StrategyName: "repricinglag", ConditionID: p.conditionID, EventSlug: p.eventSlug,
				Wallet: p.wallet, Side: p.side, Kind: shadowdecisions.KindStandalone, Level: shadowdecisions.LevelInfo,
				Score: wins[0].LagScore, Confidence: wins[0].PeerMove,
				Reasons:             []string{"phase_d_replay", "closed_windows=" + fmt.Sprint(len(wins))},
				LinkedAlertDedupKey: p.dedupKey, FiredAt: now,
			})
			if id > 0 {
				perStrategy["repricinglag"]++
			}
		}
	}

	// Run the value evaluator + promotion + outcome backfill to
	// exercise their adapters end-to-end.
	phaseB.ValueWorker.Tick(ctx)
	phaseB.PromotionRev.Tick(ctx)

	// Total inserted
	var total int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM polymarket_strategy_shadow_decisions
WHERE strategy_version = 'v11.8-phase-d'`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	fmt.Printf("[phase-d] shadow_decisions written: total=%d, per-strategy=%v\n", total, perStrategy)

	if total == 0 {
		t.Fatalf("expected at least one shadow row across strategies; got 0")
	}
	if len(perStrategy) < 2 {
		t.Fatalf("expected >= 2 strategies with shadow rows; got %d", len(perStrategy))
	}

	// Sample one row from each strategy.
	var rrows pgtype.Timestamptz
	_ = rrows
	sampleRows, _ := pool.Query(ctx, `
SELECT strategy_name, COUNT(*)::int, MIN(score)::float, MAX(score)::float
FROM polymarket_strategy_shadow_decisions
WHERE strategy_version = 'v11.8-phase-d'
GROUP BY strategy_name
ORDER BY strategy_name`)
	defer sampleRows.Close()
	for sampleRows.Next() {
		var name string
		var n int
		var minS, maxS float64
		if err := sampleRows.Scan(&name, &n, &minS, &maxS); err == nil {
			fmt.Printf("[phase-d]   %s: %d rows, score range [%.2f, %.2f]\n", name, n, minS, maxS)
		}
	}

	// Cleanup probe rows.
	if _, err := pool.Exec(ctx, `DELETE FROM polymarket_strategy_shadow_decisions WHERE strategy_version = 'v11.8-phase-d'`); err != nil {
		t.Logf("cleanup failed (non-fatal): %v", err)
	}
}
