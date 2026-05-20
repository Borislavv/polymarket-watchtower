package importer

import (
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// mkTierWorker is a minimal worker hand for the tier-classification
// tests: pure logic, no DB, no AI, no metrics.
func mkTierWorker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	cfg.Enabled = true
	cfg.applyDefaults()
	return &Worker{
		cfg:         cfg,
		lastFetched: map[string]time.Time{},
	}
}

func TestClassifyTier_PoliticsCategoryWithVolumeIsTier1(t *testing.T) {
	w := mkTierWorker(t, Config{})
	c := repository.IntelligenceCandidate{Category: "Politics", Volume24hUSD: 250000, Alerts24h: 5}
	if got := w.classifyTier(c); got != 1 {
		t.Errorf("got tier=%d want 1", got)
	}
}

func TestClassifyTier_GeopoliticsCategoryAlwaysTier1(t *testing.T) {
	w := mkTierWorker(t, Config{})
	c := repository.IntelligenceCandidate{Category: "Geopolitics", Volume24hUSD: 0, Alerts24h: 0}
	if got := w.classifyTier(c); got != 1 {
		t.Errorf("got tier=%d want 1 (category override)", got)
	}
}

func TestClassifyTier_NormalPoliticalRaceIsTier2(t *testing.T) {
	w := mkTierWorker(t, Config{})
	c := repository.IntelligenceCandidate{Category: "Politics", Volume24hUSD: 15000, Alerts24h: 0}
	if got := w.classifyTier(c); got != 2 {
		t.Errorf("got tier=%d want 2", got)
	}
}

func TestClassifyTier_LowSignalIsTier3(t *testing.T) {
	w := mkTierWorker(t, Config{})
	c := repository.IntelligenceCandidate{Category: "Politics", Volume24hUSD: 200, Alerts24h: 0}
	if got := w.classifyTier(c); got != 3 {
		t.Errorf("got tier=%d want 3", got)
	}
}

func TestDueByTier_HonorsCadence(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	w := mkTierWorker(t, Config{
		TieringEnabled: true,
		Tier1Interval:  5 * time.Minute,
		Tier2Interval:  15 * time.Minute,
		Tier3Interval:  60 * time.Minute,
		Clock:          func() time.Time { return now },
	})
	// Never fetched → due.
	if !w.dueByTier("ev-a", 1) {
		t.Error("never-fetched event must be due")
	}
	w.recordFetched("ev-a")
	if w.dueByTier("ev-a", 1) {
		t.Error("just-fetched tier 1 event must NOT be due")
	}
	// Move clock forward but still inside tier 1 cadence.
	w = mkTierWorker(t, Config{
		TieringEnabled: true,
		Tier1Interval:  5 * time.Minute,
		Tier2Interval:  15 * time.Minute,
		Tier3Interval:  60 * time.Minute,
		Clock:          func() time.Time { return now.Add(2 * time.Minute) },
	})
	w.lastFetched["ev-a"] = now
	if w.dueByTier("ev-a", 1) {
		t.Error("2m later tier 1 should still be cooling")
	}
	// 6m later — past the cadence.
	w = mkTierWorker(t, Config{
		TieringEnabled: true,
		Tier1Interval:  5 * time.Minute,
		Clock:          func() time.Time { return now.Add(6 * time.Minute) },
	})
	w.lastFetched["ev-a"] = now
	if !w.dueByTier("ev-a", 1) {
		t.Error("6m later tier 1 must be due")
	}
	// Tier 3 needs 60m.
	w = mkTierWorker(t, Config{
		TieringEnabled: true,
		Tier3Interval:  60 * time.Minute,
		Clock:          func() time.Time { return now.Add(30 * time.Minute) },
	})
	w.lastFetched["ev-c"] = now
	if w.dueByTier("ev-c", 3) {
		t.Error("30m later tier 3 must still be cooling")
	}
}

func TestDueByTier_DisabledIsAlwaysDue(t *testing.T) {
	w := mkTierWorker(t, Config{
		TieringEnabled: false,
		Tier1Interval:  5 * time.Minute,
		Clock:          func() time.Time { return time.Now() },
	})
	w.recordFetched("ev-x")
	if !w.dueByTier("ev-x", 1) {
		t.Error("tiering disabled must always return due")
	}
}
