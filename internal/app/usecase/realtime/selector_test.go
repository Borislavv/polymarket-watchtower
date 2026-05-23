package realtime

import (
	"regexp"
	"strings"
	"testing"
)

// TestBuildSelectorSQL_NoSRFInsideCoalesce pins the v11.10 WS selector
// invariant: NO set-returning function (unnest / jsonb_array_elements*)
// may appear inside a COALESCE(...) or CASE expression. Postgres 14+
// rejects this with SQLSTATE 0A000:
//
//	"set-returning functions are not allowed in COALESCE"
//
// The boot-time symptom was `realtime ws: empty subscription set,
// sleeping` warns every 5 minutes. The fix routes set-returning
// expansion through LATERAL `WITH ORDINALITY` joins, not COALESCE.
func TestBuildSelectorSQL_NoSRFInsideCoalesce(t *testing.T) {
	srfRe := regexp.MustCompile(`(?i)COALESCE\s*\(\s*[^()]*(unnest|jsonb_array_elements_text|jsonb_array_elements|regexp_split_to_table)\s*\(`)
	for _, mode := range []string{"hot", "predictions", "alerts", "all_active_limited"} {
		sql := buildSelectorSQL(mode)
		if loc := srfRe.FindStringIndex(sql); loc != nil {
			t.Fatalf("mode=%s SQL contains set-returning function inside COALESCE — Postgres rejects with SQLSTATE 0A000.\nOffending fragment: %q",
				mode, sql[loc[0]:min(loc[0]+120, len(sql))])
		}
	}
}

// TestBuildSelectorSQL_UsesLateralWithOrdinality pins that the
// (token, outcome) pairing is done via WITH ORDINALITY LATERAL joins
// — the canonical safe form. If a future refactor reverts to
// array_position()/unnest tricks, this test fails before the SQL
// reaches Postgres.
func TestBuildSelectorSQL_UsesLateralWithOrdinality(t *testing.T) {
	for _, mode := range []string{"hot", "predictions", "alerts", "all_active_limited"} {
		sql := buildSelectorSQL(mode)
		if !strings.Contains(sql, "WITH ORDINALITY") {
			t.Fatalf("mode=%s SQL must pair tokens with outcomes via WITH ORDINALITY", mode)
		}
		if !strings.Contains(sql, "jsonb_array_elements_text") {
			t.Fatalf("mode=%s SQL must use jsonb_array_elements_text + ORDINALITY (got: %s)", mode, sql)
		}
	}
}

// TestBuildSelectorSQL_AlwaysLimitsByOne pins the safety invariant
// from the file comment: every variant must LIMIT $1.
func TestBuildSelectorSQL_AlwaysLimitsByOne(t *testing.T) {
	for _, mode := range []string{"hot", "predictions", "alerts", "all_active_limited"} {
		sql := buildSelectorSQL(mode)
		if !strings.Contains(sql, "LIMIT $1") {
			t.Fatalf("mode=%s SQL missing LIMIT $1 — unbounded WS subscription is unsafe", mode)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
