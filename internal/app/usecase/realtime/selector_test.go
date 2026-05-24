package realtime

import (
	"regexp"
	"strings"
	"testing"
)

// TestBuildSelectorSQL_NoSRFInsideCoalesce pins the v11.10 WS selector
// invariant: NO set-returning function (unnest / jsonb_array_elements*)
// may appear inside a COALESCE(...) or CASE expression. Postgres 14+
// rejects this with SQLSTATE 0A000.
func TestBuildSelectorSQL_NoSRFInsideCoalesce(t *testing.T) {
	srfRe := regexp.MustCompile(`(?i)COALESCE\s*\(\s*[^()]*(unnest|jsonb_array_elements_text|jsonb_array_elements|regexp_split_to_table)\s*\(`)
	for _, mode := range []string{"hot", "alerts"} {
		sql, _ := buildSelectorSQL(mode, selectorOptions{AnnotationLookbackHours: 168}, 25)
		if loc := srfRe.FindStringIndex(sql); loc != nil {
			t.Fatalf("mode=%s SQL contains set-returning function inside COALESCE — Postgres rejects with SQLSTATE 0A000.\nOffending fragment: %q",
				mode, sql[loc[0]:min(loc[0]+120, len(sql))])
		}
	}
}

// TestBuildSelectorSQL_UsesLateralWithOrdinality pins that (token,
// outcome) pairing uses WITH ORDINALITY LATERAL joins.
func TestBuildSelectorSQL_UsesLateralWithOrdinality(t *testing.T) {
	for _, mode := range []string{"hot", "alerts"} {
		sql, _ := buildSelectorSQL(mode, selectorOptions{AnnotationLookbackHours: 168}, 25)
		if !strings.Contains(sql, "WITH ORDINALITY") {
			t.Fatalf("mode=%s SQL must pair tokens with outcomes via WITH ORDINALITY", mode)
		}
		if !strings.Contains(sql, "jsonb_array_elements_text") {
			t.Fatalf("mode=%s SQL must use jsonb_array_elements_text + ORDINALITY (got: %s)", mode, sql)
		}
	}
}

// TestBuildSelectorSQL_LimitIsMarketsNotTokens pins the v11.12 invariant:
// LIMIT $1 caps MARKETS (via the `ranked` CTE), NOT the outer
// (market × token) cross-product. A token-level LIMIT would silently
// drop outcome tokens — the user's explicit failure mode.
func TestBuildSelectorSQL_LimitIsMarketsNotTokens(t *testing.T) {
	for _, mode := range []string{"hot", "alerts"} {
		sql, _ := buildSelectorSQL(mode, selectorOptions{AnnotationLookbackHours: 168}, 25)
		// The CTE that bounds the universe MUST contain LIMIT $1.
		if !strings.Contains(sql, "LIMIT $1") {
			t.Fatalf("mode=%s: LIMIT $1 missing — must cap MARKETS in the CTE", mode)
		}
		// The outer SELECT must NOT carry an additional LIMIT clause
		// — that would re-cap by (market × token) cross-product rows,
		// silently truncating outcome tokens.
		trailing := sql[strings.LastIndex(sql, ")"):]
		if strings.Contains(strings.ToUpper(trailing), "LIMIT") {
			t.Fatalf("mode=%s: outer SELECT must not LIMIT — that would truncate tokens.\nTrailing fragment: %q", mode, trailing)
		}
	}
}

// TestBuildSelectorSQL_DistinctOnPerCondition pins the v11.11 fix:
// the freshest event_page snapshot per condition_id wins.
func TestBuildSelectorSQL_DistinctOnPerCondition(t *testing.T) {
	for _, mode := range []string{"hot", "alerts"} {
		sql, _ := buildSelectorSQL(mode, selectorOptions{AnnotationLookbackHours: 168}, 25)
		if !strings.Contains(sql, "DISTINCT ON (em.condition_id)") {
			t.Fatalf("mode=%s SQL must DISTINCT ON (em.condition_id) — got: %s", mode, sql)
		}
		if !strings.Contains(sql, "ORDER BY em.condition_id, em.created_at DESC") {
			t.Fatalf("mode=%s DISTINCT ON must ORDER BY (em.condition_id, em.created_at DESC)", mode)
		}
	}
}

// TestBuildSelectorSQL_NoPredictionReferences pins the v11.12-insider-prior
// invariant: NO prediction-state input may appear in any selector
// SQL. Prediction tables (polymarket_market_predictions) are stale
// (v11.2 stopped writing to them) and were causing the WS coverage
// to silently shrink. If a future regression re-introduces them, this
// test fails before reaching production.
func TestBuildSelectorSQL_NoPredictionReferences(t *testing.T) {
	banned := []string{
		"polymarket_market_predictions",
		"polymarket_market_prediction_states",
		"watching_prediction",
		"active_catalyst_prediction",
		"blocked_or_active_catalyst",
	}
	// "blocked" as a literal keyword would also be banned — but
	// the WS event types include "best_bid_ask" which contains the
	// substring "best_bid". Stick to the prediction-specific terms.
	for _, mode := range []string{"hot", "alerts"} {
		sql, _ := buildSelectorSQL(mode, selectorOptions{
			AnnotationLookbackHours: 168,
			IncludeHighTrade:        true,
			HighTradeMinTrades24h:   50,
			HighTradeLookbackHours:  24,
		}, 25)
		low := strings.ToLower(sql)
		for _, term := range banned {
			if strings.Contains(low, strings.ToLower(term)) {
				t.Fatalf("mode=%s SQL still references prediction term %q — must be removed.\nSQL: %s",
					mode, term, sql)
			}
		}
		// current_state, the prediction enum column, must also not
		// appear anywhere.
		if strings.Contains(low, "current_state") {
			t.Fatalf("mode=%s SQL references current_state (prediction enum)", mode)
		}
	}
}

// TestBuildSelectorSQL_AnnotationLookbackHonored pins that the
// AnnotationLookbackHours option propagates into the SQL.
// Default 168h (7d) — confirmed correct after live audit of
// polymarket_event_annotations (linkup source, ~1 entry/day/event).
func TestBuildSelectorSQL_AnnotationLookbackHonored(t *testing.T) {
	sql, _ := buildSelectorSQL("hot", selectorOptions{AnnotationLookbackHours: 168}, 25)
	if !strings.Contains(sql, "INTERVAL '168 hours'") {
		t.Fatalf("hot SQL must embed annotation lookback 168 hours; got %s", sql)
	}
	sql, _ = buildSelectorSQL("hot", selectorOptions{AnnotationLookbackHours: 12}, 25)
	if !strings.Contains(sql, "INTERVAL '12 hours'") {
		t.Fatalf("hot SQL must embed configured lookback hours; got %s", sql)
	}
}

// TestBuildSelectorSQL_OperatorPinnedBucket pins the operator_pinned
// priority-1 bucket. The pinned condition_ids array is passed as $2;
// every other bucket evaluates independently.
func TestBuildSelectorSQL_OperatorPinnedBucket(t *testing.T) {
	sql, args := buildSelectorSQL("hot", selectorOptions{
		AnnotationLookbackHours:    168,
		OperatorPinnedConditionIDs: []string{"0xpinA", "0xpinB"},
	}, 25)
	if !strings.Contains(sql, "$2::text[]") {
		t.Fatalf("hot SQL must consume operator pins via $2::text[]; got %s", sql)
	}
	if !strings.Contains(sql, "1 AS priority") {
		t.Fatalf("hot SQL must emit operator_pinned bucket at priority 1; got %s", sql)
	}
	if len(args) < 2 {
		t.Fatalf("buildSelectorSQL must return 2 args (markets, pins); got %d", len(args))
	}
	pins, _ := args[1].([]string)
	if len(pins) != 2 {
		t.Fatalf("pins arg must be the configured slice; got %v", args[1])
	}
}

// TestBuildSelectorSQL_OperatorPinnedBucketNilSafe proves we never
// pass a nil slice to pgx (which would map to NULL and break
// `ANY($2::text[])`).
func TestBuildSelectorSQL_OperatorPinnedBucketNilSafe(t *testing.T) {
	_, args := buildSelectorSQL("hot", selectorOptions{AnnotationLookbackHours: 168}, 25)
	pins, ok := args[1].([]string)
	if !ok {
		t.Fatalf("pins arg must be []string; got %T", args[1])
	}
	if pins == nil {
		t.Fatalf("pins arg must NEVER be nil; pgx maps it to SQL NULL and breaks ANY(...)")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
