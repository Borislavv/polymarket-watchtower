package aggregate

import (
	"sort"
	"testing"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

func TestRegistryReplaceReportsRemoved(t *testing.T) {
	r := NewRegistry()
	r.Replace([]market.Market{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}, nil)
	removed := r.Replace([]market.Market{{ID: "b"}, {ID: "d"}}, nil)
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	if len(removed) != 2 || removed[0] != "a" || removed[1] != "c" {
		t.Fatalf("removed: %+v", removed)
	}
	if _, ok := r.Get(vo.MarketID("d")); !ok {
		t.Fatal("new market d not present")
	}
}
