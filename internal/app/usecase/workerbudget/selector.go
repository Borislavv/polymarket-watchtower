// Package workerbudget implements the v11.10 PART 6 priority-bucket
// budgeting layer for strategy-supporting workers (holdersync,
// bookbars, etc.).
//
// The contract: instead of "ORDER BY last_seen_at DESC LIMIT 250",
// each worker asks the selector for at most N markets distributed
// across six prioritised buckets — operator-pinned, recent-alert,
// catalyst-near, linked-to-fired, liquid, fallback active. Bucket
// budgets are configurable; each bucket has its own per-cycle cap so
// a fat bucket (e.g. liquid) cannot starve operator pins.
//
// Selection is one Postgres roundtrip — see sqlc query
// ListBucketedMarketTokens. The selector dedupes by condition_id
// keeping the lowest-priority bucket and emits per-bucket Prometheus
// counters so the operator can see what was selected.
package workerbudget

import (
	"context"
	"fmt"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// Budget is the per-cycle cap for each priority bucket. A zero value
// disables the bucket entirely (its LIMIT is 0).
type Budget struct {
	OperatorPinned int
	RecentAlert    int
	CatalystNear   int
	LinkedToFired  int
	Liquid         int
	FallbackActive int
	// PinnedConditionIDs is the operator-supplied list of condition
	// ids that must always be polled when present. Empty means no
	// pins. Supplied via WORKER_OPERATOR_PINNED_CONDITION_IDS.
	PinnedConditionIDs []string
}

// Total returns the maximum possible selection size — sum of all
// bucket caps. Real output is ≤ Total because of dedupe.
func (b Budget) Total() int {
	return b.OperatorPinned + b.RecentAlert + b.CatalystNear +
		b.LinkedToFired + b.Liquid + b.FallbackActive
}

// AllZero reports whether every bucket is disabled. When true the
// caller should fall back to its legacy unbucketed lister.
func (b Budget) AllZero() bool {
	return b.Total() == 0 && len(b.PinnedConditionIDs) == 0
}

// MarketToken is one (condition_id, token_id, bucket) selection row.
type MarketToken struct {
	ConditionID string
	TokenID     string
	Bucket      Bucket
}

// Bucket is the priority bucket the row was selected from. Stable
// integer values match the SQL.
type Bucket int

const (
	BucketOperatorPinned Bucket = 1
	BucketRecentAlert    Bucket = 2
	BucketCatalystNear   Bucket = 3
	BucketLinkedToFired  Bucket = 4
	BucketLiquid         Bucket = 5
	BucketFallbackActive Bucket = 6
)

// Name returns the metric / log label.
func (b Bucket) Name() string {
	switch b {
	case BucketOperatorPinned:
		return "operator_pinned"
	case BucketRecentAlert:
		return "recent_alert"
	case BucketCatalystNear:
		return "catalyst_near"
	case BucketLinkedToFired:
		return "linked_to_fired"
	case BucketLiquid:
		return "liquid"
	case BucketFallbackActive:
		return "fallback_active"
	}
	return "unknown"
}

// Result is the deduped selection + per-bucket counts.
type Result struct {
	Rows        []MarketToken
	PerBucket   map[Bucket]int
	TotalUnique int
}

// Lister is the read-side dependency. Production wires sqlc.Queries.
type Lister interface {
	ListBucketedMarketTokens(ctx context.Context, arg sqlc.ListBucketedMarketTokensParams) ([]sqlc.ListBucketedMarketTokensRow, error)
}

// Selector executes the bucketed selection.
type Selector struct {
	lister Lister
}

func New(lister Lister) *Selector { return &Selector{lister: lister} }

// Select issues ONE bucketed query and returns the deduped rows plus
// per-bucket counts. When the budget is all-zero the caller is
// responsible for falling back to the unbucketed lister; Select
// returns an empty Result in that case so the worker can decide.
func (s *Selector) Select(ctx context.Context, b Budget) (Result, error) {
	if s == nil || s.lister == nil {
		return Result{PerBucket: map[Bucket]int{}}, nil
	}
	if b.AllZero() {
		return Result{PerBucket: map[Bucket]int{}}, nil
	}
	pins := normalisePins(b.PinnedConditionIDs)
	rows, err := s.lister.ListBucketedMarketTokens(ctx, sqlc.ListBucketedMarketTokensParams{
		PinnedConditionIds: pins,
		PinnedLimit:        int32(b.OperatorPinned),
		RecentAlertLimit:   int32(b.RecentAlert),
		CatalystNearLimit:  int32(b.CatalystNear),
		LinkedToFiredLimit: int32(b.LinkedToFired),
		LiquidLimit:        int32(b.Liquid),
		FallbackLimit:      int32(b.FallbackActive),
	})
	if err != nil {
		return Result{PerBucket: map[Bucket]int{}}, fmt.Errorf("workerbudget.Select: %w", err)
	}
	out := Result{
		Rows:      make([]MarketToken, 0, len(rows)),
		PerBucket: map[Bucket]int{},
	}
	seenCond := map[string]struct{}{}
	for _, r := range rows {
		bucket := Bucket(r.Bucket)
		out.Rows = append(out.Rows, MarketToken{
			ConditionID: r.ConditionID,
			TokenID:     r.TokenID,
			Bucket:      bucket,
		})
		if _, ok := seenCond[r.ConditionID]; !ok {
			out.PerBucket[bucket]++
			seenCond[r.ConditionID] = struct{}{}
			out.TotalUnique++
		}
	}
	return out, nil
}

// normalisePins lowercases hex prefixes if needed and trims whitespace.
// We pass through as-is for non-hex strings — Polymarket uses 0x-
// prefixed condition ids but our DB stores them verbatim.
func normalisePins(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
