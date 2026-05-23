package workerbudget

import (
	"context"
	"errors"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

type stubLister struct {
	rows   []sqlc.ListBucketedMarketTokensRow
	err    error
	called bool
	got    sqlc.ListBucketedMarketTokensParams
}

func (s *stubLister) ListBucketedMarketTokens(_ context.Context, arg sqlc.ListBucketedMarketTokensParams) ([]sqlc.ListBucketedMarketTokensRow, error) {
	s.called = true
	s.got = arg
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// TestSelector_AllZeroBudgetSkipsQuery pins the v11.10 invariant:
// when every bucket cap is 0 AND no pins are supplied, the selector
// returns an empty Result without hitting the DB so the worker can
// fall back to its legacy unbucketed query path.
func TestSelector_AllZeroBudgetSkipsQuery(t *testing.T) {
	s := &stubLister{}
	sel := New(s)
	res, err := sel.Select(context.Background(), Budget{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if s.called {
		t.Fatalf("DB lister must NOT be called when budget is all-zero")
	}
	if len(res.Rows) != 0 || res.TotalUnique != 0 {
		t.Fatalf("zero-budget must return empty result, got %+v", res)
	}
}

// TestSelector_DedupCountsKeepLowestBucket pins the dedupe rule:
// when one condition_id ends up in multiple buckets (the SQL already
// dedupes, but the Go counter must not double-count tokens), the
// PerBucket counter only increments once per condition_id at its
// MIN(bucket).
func TestSelector_DedupCountsKeepLowestBucket(t *testing.T) {
	s := &stubLister{
		rows: []sqlc.ListBucketedMarketTokensRow{
			// market A — pinned, with 2 outcome tokens
			{ConditionID: "A", TokenID: "tokA1", Bucket: 1},
			{ConditionID: "A", TokenID: "tokA2", Bucket: 1},
			// market B — recent_alert, 1 token
			{ConditionID: "B", TokenID: "tokB", Bucket: 2},
			// market C — catalyst_near, 2 tokens
			{ConditionID: "C", TokenID: "tokC1", Bucket: 3},
			{ConditionID: "C", TokenID: "tokC2", Bucket: 3},
		},
	}
	res, err := New(s).Select(context.Background(), Budget{
		OperatorPinned: 5, RecentAlert: 5, CatalystNear: 5,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.TotalUnique != 3 {
		t.Fatalf("expected 3 unique markets, got %d", res.TotalUnique)
	}
	if got := res.PerBucket[BucketOperatorPinned]; got != 1 {
		t.Fatalf("bucket=operator_pinned count = %d, want 1 (one unique condition_id, two tokens)", got)
	}
	if got := res.PerBucket[BucketRecentAlert]; got != 1 {
		t.Fatalf("bucket=recent_alert count = %d, want 1", got)
	}
	if got := res.PerBucket[BucketCatalystNear]; got != 1 {
		t.Fatalf("bucket=catalyst_near count = %d, want 1", got)
	}
	if len(res.Rows) != 5 {
		t.Fatalf("expected 5 token rows total, got %d", len(res.Rows))
	}
}

// TestSelector_PinsAreNormalised pins that whitespace + duplicate pins
// are removed before they reach the SQL.
func TestSelector_PinsAreNormalised(t *testing.T) {
	s := &stubLister{}
	_, _ = New(s).Select(context.Background(), Budget{
		OperatorPinned:     5,
		PinnedConditionIDs: []string{" 0xabc ", "0xabc", "", "0xdef"},
	})
	if !s.called {
		t.Fatalf("lister must be called when pins are present")
	}
	if got, want := len(s.got.PinnedConditionIds), 2; got != want {
		t.Fatalf("normalised pins len = %d, want %d (got %v)", got, want, s.got.PinnedConditionIds)
	}
}

// TestSelector_PropagatesError pins that DB errors are surfaced
// (workers can then fall back to their legacy path).
func TestSelector_PropagatesError(t *testing.T) {
	s := &stubLister{err: errors.New("boom")}
	_, err := New(s).Select(context.Background(), Budget{OperatorPinned: 1})
	if err == nil {
		t.Fatal("expected error from lister to propagate")
	}
}

func TestBucket_Name(t *testing.T) {
	cases := []struct {
		b    Bucket
		want string
	}{
		{BucketOperatorPinned, "operator_pinned"},
		{BucketRecentAlert, "recent_alert"},
		{BucketCatalystNear, "catalyst_near"},
		{BucketLinkedToFired, "linked_to_fired"},
		{BucketLiquid, "liquid"},
		{BucketFallbackActive, "fallback_active"},
		{Bucket(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.b.Name(); got != c.want {
			t.Errorf("Bucket(%d).Name() = %q, want %q", c.b, got, c.want)
		}
	}
}
