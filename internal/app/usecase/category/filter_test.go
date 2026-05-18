package category

import "testing"

func TestEmptyWhitelistAllowsEverything(t *testing.T) {
	f := NewFilter(nil)
	if !f.Allowed("anything", "Anything Goes") {
		t.Fatal("empty whitelist must allow everything (filter disabled)")
	}
	if f.Summary() != "disabled" {
		t.Fatalf("summary: %s", f.Summary())
	}
}

func TestWhitelistMatchesSubstring(t *testing.T) {
	f := NewFilter([]string{"politics"})
	if !f.Allowed("politics", "Politics") {
		t.Fatal("politics category must pass when whitelisted")
	}
	if !f.Allowed("us-politics", "US Politics") {
		t.Fatal("substring match must accept yearly/region variants")
	}
	if f.Allowed("sports", "Sports") {
		t.Fatal("sports must be blocked when not whitelisted")
	}
	if f.Allowed("nba", "2026 NBA Playoffs") {
		t.Fatal("non-whitelisted league must be blocked")
	}
}

func TestWhitelistDefaultPolitics(t *testing.T) {
	// The shipped default. Verifies the operator's go-to setup actually
	// admits Politics and rejects the common noise categories.
	f := NewFilter([]string{"Politics"})
	allowed := []struct{ slug, label string }{
		{"politics", "Politics"},
		{"us-politics", "US Politics"},
		{"2026-elections", "2026 Elections (Politics)"},
	}
	blocked := []struct{ slug, label string }{
		{"sports", "Sports"},
		{"crypto", "Crypto"},
		{"culture", "Culture"},
		{"weather", "Weather"},
		{"nba", "NBA"},
	}
	for _, c := range allowed {
		if !f.Allowed(c.slug, c.label) {
			t.Errorf("politics-shaped %q/%q must pass", c.slug, c.label)
		}
	}
	for _, c := range blocked {
		if f.Allowed(c.slug, c.label) {
			t.Errorf("non-politics %q/%q must be blocked", c.slug, c.label)
		}
	}
}

func TestCaseInsensitive(t *testing.T) {
	f := NewFilter([]string{"POLITICS"})
	if !f.Allowed("politics", "Politics") {
		t.Fatal("must be case-insensitive on input")
	}
	if !f.Allowed("POLITICS", "POLITICS") {
		t.Fatal("must be case-insensitive on label")
	}
}

func TestSlugMatchedSeparatelyFromLabel(t *testing.T) {
	f := NewFilter([]string{"politics"})
	// Slug carries the hint, label is empty.
	if !f.Allowed("politics-uk", "") {
		t.Fatal("slug-based match must work")
	}
	// Label carries the hint, slug is empty.
	if !f.Allowed("", "Politics & Power") {
		t.Fatal("label-based match must work")
	}
}

func TestWhitespaceAndEmptyTokensIgnored(t *testing.T) {
	f := NewFilter([]string{" ", "", "  Politics  ", "\tmacro\n"})
	got := f.Tokens()
	if len(got) != 2 || got[0] != "politics" || got[1] != "macro" {
		t.Fatalf("tokens normalised: %v", got)
	}
}

// TestUncategorisedBlockedUnderActiveWhitelist locks in the semantic
// change: with a blacklist, an uncategorised market would pass (no token
// to match against); under a whitelist, it must be blocked because it
// cannot affirmatively match anything we asked for.
func TestUncategorisedBlockedUnderActiveWhitelist(t *testing.T) {
	f := NewFilter([]string{"politics"})
	if f.Allowed("", "") {
		t.Fatal("uncategorised market must be blocked when whitelist is active")
	}
}

func TestSummaryListsActiveTokens(t *testing.T) {
	f := NewFilter([]string{"Politics", "Macro"})
	if got := f.Summary(); got != "politics,macro" {
		t.Fatalf("summary: %s", got)
	}
}
