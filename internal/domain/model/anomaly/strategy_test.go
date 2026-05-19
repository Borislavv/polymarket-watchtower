package anomaly

import (
	"strings"
	"testing"
)

// TestStrategyIdentityIsV6 pins the current dedup namespace. Bumping
// this string is a deliberate decision (see CLAUDE.md "When to bump")
// — failing this test is a reminder to update consumer dedup paths
// before changing it.
func TestStrategyIdentityIsV6(t *testing.T) {
	if StrategyIdentity != "informed-flow-v6" {
		t.Fatalf("strategy identity drift: got %q want informed-flow-v6", StrategyIdentity)
	}
}

// TestStrategyIdentityNoLegacyReferences guards against accidental
// reintroduction of older identities anywhere in the constant. A
// hybrid like "informed-flow-v4-v6-compat" would silently break
// dedup; reject it.
func TestStrategyIdentityNoLegacyReferences(t *testing.T) {
	for _, legacy := range []string{"v1", "v2", "v3", "v4", "v5"} {
		if strings.Contains(StrategyIdentity, "-"+legacy) {
			t.Errorf("strategy identity contains legacy reference %q: %q", legacy, StrategyIdentity)
		}
	}
}
