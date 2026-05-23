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
}

// Input is the pure-Decide payload.
type Input struct {
	ConditionID   string
	EventSlug     string
	AlertWallet   string
	AlertSide     string
	RecentMembers []CohortMember // wallets that co-entered the candidate market recently
	Edges         []Edge         // edges sourced from the AlertWallet
}

// CohortVerdict is the pure-Decide output. Booster only.
type CohortVerdict struct {
	CohortID      string
	CohortSize    int
	SimilarityAvg float64
	Converged     bool
	Boost         float64
	Reasons       []string
	Features      map[string]any
}

// Config tunes Decide().
type Config struct {
	MinSimilarity float64 // default 0.6
	MinEvents     int     // default 3
	MinCohortSize int     // default 2 (alert wallet + at least one peer)
	MaxBoost      float64 // default 6
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
func (d *Detector) Decide(in Input) CohortVerdict {
	v := CohortVerdict{Features: map[string]any{}}

	// Build the set of cohort peers from edges that clear the
	// floors.
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
	if len(peers) == 0 {
		v.Reasons = []string{"no_qualifying_edges"}
		return v
	}
	// Convergence: peers entered the same market on the same
	// side recently.
	convergedWallets := make([]string, 0)
	var simSum float64
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
	cohortSize := len(convergedWallets) + 1 // +1 for alert wallet
	if cohortSize < d.cfg.MinCohortSize {
		v.Reasons = []string{"no_recent_convergence"}
		return v
	}
	v.Converged = true
	v.CohortSize = cohortSize
	v.SimilarityAvg = simSum / float64(len(convergedWallets))
	// Strongest cohort_id label (the one shared by most edges).
	bestID := ""
	bestN := 0
	for id, n := range cohortByID {
		if n > bestN {
			bestN = n
			bestID = id
		}
	}
	v.CohortID = bestID

	// Boost grows with cohort size + similarity.
	v.Boost = d.cfg.MaxBoost * (v.SimilarityAvg) * float64(cohortSize-1) / 3.0
	if v.Boost > d.cfg.MaxBoost {
		v.Boost = d.cfg.MaxBoost
	}
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("cohort_size=%d avg_sim=%.2f peers=%v",
			v.CohortSize, v.SimilarityAvg, convergedWallets))
	v.Features["cohort_size"] = v.CohortSize
	v.Features["avg_similarity"] = v.SimilarityAvg
	v.Features["converged_peers"] = convergedWallets
	v.Features["boost"] = v.Boost
	return v
}
