package category

import "testing"

func TestEmptyBlacklistAllowsEverything(t *testing.T) {
	f := NewFilter(nil)
	if !f.Allowed("anything", "Anything Goes") {
		t.Fatal("empty blacklist must allow")
	}
	if f.Summary() != "disabled" {
		t.Fatalf("summary: %s", f.Summary())
	}
}

func TestTokenMatchesSubstring(t *testing.T) {
	f := NewFilter([]string{"sport"})
	if !f.Allowed("politics", "Politics") {
		t.Fatal("politics must pass")
	}
	if f.Allowed("sports", "Sports") {
		t.Fatal("sports must be blocked")
	}
}

func TestSubstringMatchesYearPrefixedLabel(t *testing.T) {
	f := NewFilter([]string{"nba", "nhl", "fifa", "uefa", "champions league", "stanley cup"})
	cases := []struct {
		label   string
		blocked bool
	}{
		{"2026 NBA Playoffs", true},
		{"2026 NHL Playoffs", true},
		{"2026 FIFA World Cup", true},
		{"UEFA Champions League Winner", true},
		{"Champions League Top Scorer", true},
		{"Stanley Cup", true},
		{"US Election", false},
		{"Politics", false},
		{"Crypto Prices", false},
	}
	for _, c := range cases {
		got := !f.Allowed("", c.label)
		if got != c.blocked {
			t.Errorf("label=%q blocked=%v want %v", c.label, got, c.blocked)
		}
	}
}

func TestCaseInsensitive(t *testing.T) {
	f := NewFilter([]string{"SOCCER"})
	if f.Allowed("soccer", "Soccer") {
		t.Fatal("must be case-insensitive on input")
	}
	if f.Allowed("SOCCER", "SOCCER") {
		t.Fatal("must be case-insensitive on label")
	}
}

func TestSlugMatchedSeparatelyFromLabel(t *testing.T) {
	f := NewFilter([]string{"sport"})
	// Slug carries the hint, label is empty.
	if f.Allowed("sports-uk", "") {
		t.Fatal("slug-based block must work")
	}
	// Label carries the hint, slug is empty.
	if f.Allowed("", "Sportsbook") {
		t.Fatal("label-based block must work")
	}
}

func TestWhitespaceAndEmptyTokensIgnored(t *testing.T) {
	f := NewFilter([]string{" ", "", "  Sport  ", "\tnba\n"})
	if got := f.Tokens(); len(got) != 2 || got[0] != "sport" || got[1] != "nba" {
		t.Fatalf("tokens normalised: %v", got)
	}
}

func TestBothEmptyAllowed(t *testing.T) {
	f := NewFilter([]string{"sport"})
	// No slug, no label — we cannot decide; allow to avoid losing signal.
	if !f.Allowed("", "") {
		t.Fatal("uncategorised should pass even when blacklist non-empty")
	}
}

func TestSummaryListsActiveTokens(t *testing.T) {
	f := NewFilter([]string{"NBA", "NHL"})
	if got := f.Summary(); got != "nba,nhl" {
		t.Fatalf("summary: %s", got)
	}
}
