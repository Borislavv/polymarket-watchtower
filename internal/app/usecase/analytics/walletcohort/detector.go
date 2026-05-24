// Package walletcohort implements the v11.5 Wallet Cohort /
// Shared-Funding Convergence BOOSTER. Phase A uses repeated
// co-entry behavior alone (no external chain data). Phase B
// (optional) augments cohort edges with shared-funding evidence
// from an external provider.
//
// Detector is PURE. Orchestration loads
// polymarket_wallet_graph_edges and the recent co-entry rows for
// the candidate market, then calls Decide().
package walletcohort

import (
	"fmt"
	"sort"
	"time"
)

// Edge is one wallet-pair edge from polymarket_wallet_graph_edges.
type Edge struct {
	WalletA         string
	WalletB         string
	EdgeKind        string // "co_trade" | "shared_funding" | etc.
	SimilarityScore float64
	CoEventsCount   int
	CohortID        string
}

// CohortMember is a wallet that already has confirmed activity
// on the SAME side of the candidate market within the recent
// window.
type CohortMember struct {
	Wallet string
	Side   string
	// FirstSeenAt is when the trader_id was first observed by
	// Watchtower. Zero-value = unknown. Used by the fresh-wallet
	// burst branch (v11.10) to detect insider-prior style
	// convergence even when historical edge density is weak.
	FirstSeenAt time.Time
	// EnteredAt is when this wallet entered the candidate market.
	// Zero-value = unknown.
	EnteredAt time.Time
}

// Input is the pure-Decide payload.
type Input struct {
	ConditionID    string
	EventSlug      string
	AlertWallet    string
	AlertSide      string
	AlertEnteredAt time.Time      // when the alert wallet entered the candidate market
	Now            time.Time      // for fresh-wallet age check; zero → no fresh branch
	RecentMembers  []CohortMember // wallets that co-entered the candidate market recently
	Edges          []Edge         // edges sourced from the AlertWallet
}

// CohortVerdict is the pure-Decide output. Booster only.
type CohortVerdict struct {
	CohortID      string
	CohortSize    int
	SimilarityAvg float64
	Converged     bool
	Boost         float64
	// BranchHit records which detection branch produced the verdict:
	// "edge_density" (classic Phase A) or "fresh_wallet_burst" (v11.10).
	BranchHit string
	Reasons   []string
	Features  map[string]any
}

// Config tunes Decide().
type Config struct {
	MinSimilarity float64 // default 0.6
	MinEvents     int     // default 3
	MinCohortSize int     // default 2 (alert wallet + at least one peer)
	MaxBoost      float64 // default 6
	// FreshWalletMinBurst — v11.10 fresh-wallet branch. Requires at
	// least this many wallets first_seen_at ≤ FreshWalletMaxAge that
	// converge same-side on the candidate condition within the
	// orchestration-supplied convergence window. Default 3.
	FreshWalletMinBurst int
	// FreshWalletMaxAge — first_seen_at threshold for "fresh".
	// Default 24h.
	FreshWalletMaxAge time.Duration
}

func (c *Config) applyDefaults() {
	if c.MinSimilarity <= 0 {
		c.MinSimilarity = 0.6
	}
	if c.MinEvents <= 0 {
		c.MinEvents = 3
	}
	if c.MinCohortSize <= 0 {
		c.MinCohortSize = 2
	}
	if c.MaxBoost <= 0 {
		c.MaxBoost = 6
	}
	if c.FreshWalletMinBurst <= 0 {
		c.FreshWalletMinBurst = 3
	}
	if c.FreshWalletMaxAge <= 0 {
		c.FreshWalletMaxAge = 24 * time.Hour
	}
}

// Detector is the pure verdict producer.
type Detector struct {
	cfg Config
}

func New(cfg Config) *Detector {
	cfg.applyDefaults()
	return &Detector{cfg: cfg}
}

// Decide judges whether the alert wallet is part of a confirmed
// cohort that's converging on the same market side.
//
// Two branches:
//
//   - edge_density (Phase A) — historical co-trade similarity edges
//     must clear MinSimilarity + MinEvents AND ≥ MinCohortSize-1
//     peers converged on the same side. This is the classic
//     wallet-cohort signal: known repeat co-trading wallets pile
//     together.
//
//   - fresh_wallet_burst (v11.10) — even when historical edges are
//     weak or absent, if ≥ FreshWalletMinBurst wallets with
//     first_seen_at ≤ Now-FreshWalletMaxAge converge same-side on
//     the candidate condition, the booster fires. This is the
//     insider-prior pattern: brand-new wallets show up together to
//     stake the same side. The boost is scaled down (≤ MaxBoost/2)
//     because the absence of historical co-trade evidence is a
//     weaker signal individually — but the structural fact of
//     fresh-wallet convergence on a specific side is what insider-
//     prior surveillance is trying to catch.
//
// Either branch alone is sufficient to fire.
func (d *Detector) Decide(in Input) CohortVerdict {
	v := CohortVerdict{Features: map[string]any{}}

	// ---- Branch 1: edge_density (classic Phase A) -----------------
	peers := make(map[string]float64, len(in.Edges))
	cohortByID := make(map[string]int)
	for _, e := range in.Edges {
		if e.SimilarityScore < d.cfg.MinSimilarity {
			continue
		}
		if e.CoEventsCount < d.cfg.MinEvents {
			continue
		}
		other := e.WalletB
		if other == in.AlertWallet {
			other = e.WalletA
		}
		if other == in.AlertWallet {
			continue
		}
		peers[other] = e.SimilarityScore
		if e.CohortID != "" {
			cohortByID[e.CohortID]++
		}
	}
	convergedWallets := make([]string, 0)
	var simSum float64
	if len(peers) > 0 {
		for _, m := range in.RecentMembers {
			if m.Wallet == in.AlertWallet {
				continue
			}
			if sim, ok := peers[m.Wallet]; ok && m.Side == in.AlertSide {
				convergedWallets = append(convergedWallets, m.Wallet)
				simSum += sim
			}
		}
		sort.Strings(convergedWallets)
		if cohortSize := len(convergedWallets) + 1; cohortSize >= d.cfg.MinCohortSize {
			v.Converged = true
			v.BranchHit = "edge_density"
			v.CohortSize = cohortSize
			v.SimilarityAvg = simSum / float64(len(convergedWallets))
			bestID, bestN := "", 0
			for id, n := range cohortByID {
				if n > bestN {
					bestN = n
					bestID = id
				}
			}
			v.CohortID = bestID
			v.Boost = d.cfg.MaxBoost * v.SimilarityAvg * float64(cohortSize-1) / 3.0
			if v.Boost > d.cfg.MaxBoost {
				v.Boost = d.cfg.MaxBoost
			}
			v.Reasons = append(v.Reasons,
				fmt.Sprintf("branch=edge_density cohort_size=%d avg_sim=%.2f peers=%v",
					v.CohortSize, v.SimilarityAvg, convergedWallets))
			v.Features["branch"] = "edge_density"
			v.Features["cohort_size"] = v.CohortSize
			v.Features["avg_similarity"] = v.SimilarityAvg
			v.Features["converged_peers"] = convergedWallets
			v.Features["boost"] = v.Boost
			return v
		}
	}

	// ---- Branch 2: fresh_wallet_burst (v11.10) --------------------
	// Insider-prior pattern: multiple brand-new wallets converging
	// same-side on a single condition_id in a tight window. The
	// orchestration layer feeds RecentMembers windowed to the
	// configured ConvergenceWindow.
	if !in.Now.IsZero() {
		freshAgeCutoff := in.Now.Add(-d.cfg.FreshWalletMaxAge)
		freshConverged := make([]string, 0)
		for _, m := range in.RecentMembers {
			if m.Wallet == in.AlertWallet {
				continue
			}
			if m.Side != in.AlertSide {
				continue
			}
			if m.FirstSeenAt.IsZero() || m.FirstSeenAt.Before(freshAgeCutoff) {
				continue
			}
			freshConverged = append(freshConverged, m.Wallet)
		}
		// Count the alert wallet itself if it is also fresh (the most
		// common insider-prior shape).
		alertIsFresh := !in.AlertEnteredAt.IsZero() && !in.AlertEnteredAt.Before(freshAgeCutoff)
		burstSize := len(freshConverged)
		if alertIsFresh {
			burstSize++
		}
		if burstSize >= d.cfg.FreshWalletMinBurst {
			sort.Strings(freshConverged)
			v.Converged = true
			v.BranchHit = "fresh_wallet_burst"
			v.CohortSize = burstSize
			// Conservative boost — half of MaxBoost, scaled by how far
			// burst clears the floor. Historical co-trade evidence is
			// absent in this branch by definition, so we don't blow up
			// the boost above 0.5*MaxBoost.
			v.Boost = (d.cfg.MaxBoost / 2.0) *
				float64(burstSize-d.cfg.FreshWalletMinBurst+1) / 3.0
			if v.Boost > d.cfg.MaxBoost/2.0 {
				v.Boost = d.cfg.MaxBoost / 2.0
			}
			v.Reasons = append(v.Reasons,
				fmt.Sprintf("branch=fresh_wallet_burst burst_size=%d max_age=%s peers=%v",
					burstSize, d.cfg.FreshWalletMaxAge, freshConverged))
			v.Features["branch"] = "fresh_wallet_burst"
			v.Features["burst_size"] = burstSize
			v.Features["fresh_peers"] = freshConverged
			v.Features["alert_is_fresh"] = alertIsFresh
			v.Features["boost"] = v.Boost
			return v
		}
	}

	// Neither branch fired.
	if len(peers) == 0 {
		v.Reasons = []string{"no_qualifying_edges_and_no_fresh_burst"}
	} else {
		v.Reasons = []string{"no_recent_convergence"}
	}
	return v
}
