package eventpage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// buildPayload wraps a markets array into the dehydrated-state shape
// the production parser expects. The event-slug query is the only
// one we exercise here; sibling queries (annotations etc.) are
// orthogonal.
func buildPayload(t *testing.T, marketsJSON string) []byte {
	t.Helper()
	doc := `{
	  "pageProps":{
	    "dehydratedState":{
	      "queries":[
	        {
	          "queryKey":["/api/event/slug","tx"],
	          "state":{"data":{"id":"e1","slug":"tx","title":"TX","markets":` + marketsJSON + `}}
	        }
	      ]
	    }
	  }
	}`
	return []byte(doc)
}

// TestParse_VolumeAsString covers the production-observed drift where
// market.volume / liquidity arrive as JSON strings instead of numbers.
// The legacy parser dropped the entire markets[] on this; the
// hardened parser must extract the value and stamp a type_drift
// warning.
func TestParse_VolumeAsString(t *testing.T) {
	raw := buildPayload(t, `[{
	  "id":"m1","conditionId":"0xa","slug":"m","question":"Q","outcomes":["Yes","No"],
	  "volume":"4701578.76","liquidity":"114892.94","volume24hr":212889.73,
	  "lastTradePrice":0.95,"outcomePrices":["0.945","0.055"]
	}]`)
	pl, err := parsePayload("tx", raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pl.Markets) != 1 {
		t.Fatalf("markets: got %d want 1", len(pl.Markets))
	}
	m := pl.Markets[0]
	if m.Volume != 4701578.76 {
		t.Errorf("volume: got %v want 4701578.76", m.Volume)
	}
	if m.Liquidity != 114892.94 {
		t.Errorf("liquidity: got %v want 114892.94", m.Liquidity)
	}
	if m.Volume24h != 212889.73 {
		t.Errorf("volume24h: got %v want 212889.73", m.Volume24h)
	}
	if m.LastTradePrice == nil || *m.LastTradePrice != 0.95 {
		t.Errorf("lastTradePrice: %v", m.LastTradePrice)
	}
	if pl.ParseStatus != "partial" {
		t.Errorf("status: got %q want partial", pl.ParseStatus)
	}
	wantFields := map[string]bool{"market.volume": true, "market.liquidity": true}
	got := map[string]int{}
	for _, w := range pl.ParseWarnings {
		if w.Kind == "type_drift" {
			got[w.Field]++
		}
	}
	for f := range wantFields {
		if got[f] != 1 {
			t.Errorf("missing type_drift warning for %s; warnings=%v", f, pl.ParseWarnings)
		}
	}
}

// TestParse_OutcomesAsJSONEncodedString covers the second
// production-observed drift: `outcomes` / `outcomePrices` /
// `clobTokenIds` arrive as JSON-encoded array strings.
func TestParse_OutcomesAsJSONEncodedString(t *testing.T) {
	raw := buildPayload(t, `[{
	  "id":"m1","conditionId":"0xa","slug":"m","question":"Q",
	  "volume":1,"liquidity":1,
	  "outcomes":"[\"Yes\",\"No\"]",
	  "outcomePrices":"[\"0.6\",\"0.4\"]",
	  "clobTokenIds":"[\"tok1\",\"tok2\"]"
	}]`)
	pl, err := parsePayload("tx", raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pl.Markets) != 1 {
		t.Fatalf("markets: got %d want 1", len(pl.Markets))
	}
	m := pl.Markets[0]
	want := []string{"Yes", "No"}
	if !equalStringSlice(m.Outcomes, want) {
		t.Errorf("outcomes: got %v want %v", m.Outcomes, want)
	}
	if !equalStringSlice(m.OutcomePrices, []string{"0.6", "0.4"}) {
		t.Errorf("outcomePrices: %v", m.OutcomePrices)
	}
	if !equalStringSlice(m.CLOBTokenIDs, []string{"tok1", "tok2"}) {
		t.Errorf("clobTokenIds: %v", m.CLOBTokenIDs)
	}
	if pl.ParseStatus != "partial" {
		t.Errorf("status: got %q want partial", pl.ParseStatus)
	}
	expected := []string{"market.outcomes", "market.outcomePrices", "market.clobTokenIds"}
	got := map[string]bool{}
	for _, w := range pl.ParseWarnings {
		if w.Kind == "type_drift" && w.OffendingType == "encoded_string" {
			got[w.Field] = true
		}
	}
	for _, f := range expected {
		if !got[f] {
			t.Errorf("missing type_drift/encoded_string warning for %s; warnings=%v", f, pl.ParseWarnings)
		}
	}
}

// TestParse_MalformedMarket_IsolatesFailure covers the load-bearing
// guarantee: one broken market row does NOT lose the rest of the
// array. Previously, a single market that failed to decode silently
// nuked the entire event's markets[].
func TestParse_MalformedMarket_IsolatesFailure(t *testing.T) {
	raw := buildPayload(t, `[
	  {"id":"m1","conditionId":"0xa","slug":"m","question":"Q","volume":1.5,"outcomes":["Yes","No"]},
	  {"id":"m2","conditionId":"0xb","slug":"m","question":"Q","volume":"NOT_A_NUMBER"},
	  {"id":"m3","conditionId":"0xc","slug":"m","question":"Q","volume":2.7,"outcomes":["Yes","No"]}
	]`)
	pl, err := parsePayload("tx", raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pl.Markets) != 2 {
		t.Fatalf("markets: got %d want 2", len(pl.Markets))
	}
	if pl.Markets[0].ConditionID != "0xa" || pl.Markets[1].ConditionID != "0xc" {
		t.Errorf("wrong markets kept: %+v", pl.Markets)
	}
	if pl.ParseStatus != "partial" {
		t.Errorf("status: got %q want partial", pl.ParseStatus)
	}
	var skipped int
	for _, w := range pl.ParseWarnings {
		if w.Kind == "subobject_skipped" {
			skipped++
		}
	}
	if skipped != 1 {
		t.Errorf("subobject_skipped count: got %d want 1; warnings=%v", skipped, pl.ParseWarnings)
	}
}

// TestParse_NullableFields_Tolerated checks that `null` and missing
// pointer fields keep nil pointers (the existing contract) under the
// flex types — no spurious zero-pointers.
func TestParse_NullableFields_Tolerated(t *testing.T) {
	raw := buildPayload(t, `[{
	  "id":"m1","conditionId":"0xa","slug":"m","question":"Q","volume":1,
	  "oneHourPriceChange":null,
	  "oneDayPriceChange":0.32,
	  "lastTradePrice":null,
	  "bestBid":null,
	  "bestAsk":null
	}]`)
	pl, err := parsePayload("tx", raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pl.Markets) != 1 {
		t.Fatalf("markets: got %d want 1", len(pl.Markets))
	}
	m := pl.Markets[0]
	if m.OneHourPriceChange != nil {
		t.Errorf("oneHourPriceChange expected nil, got %v", *m.OneHourPriceChange)
	}
	if m.OneDayPriceChange == nil || *m.OneDayPriceChange != 0.32 {
		t.Errorf("oneDayPriceChange: %v", m.OneDayPriceChange)
	}
	if m.LastTradePrice != nil || m.BestBid != nil || m.BestAsk != nil {
		t.Error("expected nullable price pointers to stay nil")
	}
	// A clean payload should produce status="ok" with no warnings.
	if pl.ParseStatus != "ok" {
		t.Errorf("status: got %q want ok (no drift)", pl.ParseStatus)
	}
}

// TestParse_PriceAsString covers the *float64 fields drifting to
// string. Production-observed less often but still seen for
// lastTradePrice in low-liquidity markets.
func TestParse_PriceAsString(t *testing.T) {
	raw := buildPayload(t, `[{
	  "id":"m1","conditionId":"0xa","slug":"m","question":"Q","volume":1,
	  "lastTradePrice":"0.945",
	  "bestBid":"0.940",
	  "bestAsk":"0.950"
	}]`)
	pl, err := parsePayload("tx", raw, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pl.Markets) != 1 {
		t.Fatalf("markets: got %d want 1", len(pl.Markets))
	}
	m := pl.Markets[0]
	if m.LastTradePrice == nil || *m.LastTradePrice != 0.945 {
		t.Errorf("lastTradePrice drifted-string: %v", m.LastTradePrice)
	}
	if m.BestBid == nil || *m.BestBid != 0.940 {
		t.Errorf("bestBid drifted-string: %v", m.BestBid)
	}
}

// TestParse_PartialStatusFlag pins the contract that the
// EventPagePayload tells the caller whether to expect ParseWarnings.
// This is what observeParseWarnings reads in production.
func TestParse_PartialStatusFlag(t *testing.T) {
	// One clean market => status=ok.
	clean := buildPayload(t, `[{"id":"m1","conditionId":"0xa","slug":"m","question":"Q","volume":1.5}]`)
	pl, err := parsePayload("tx", clean, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pl.ParseStatus != "ok" || len(pl.ParseWarnings) != 0 {
		t.Errorf("clean payload: got status=%q warnings=%v", pl.ParseStatus, pl.ParseWarnings)
	}
	// One drifted market => status=partial + at least one warning.
	dirty := buildPayload(t, `[{"id":"m1","conditionId":"0xa","slug":"m","question":"Q","volume":"1.5"}]`)
	pl, err = parsePayload("tx", dirty, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pl.ParseStatus != "partial" || len(pl.ParseWarnings) == 0 {
		t.Errorf("drifted payload: got status=%q warnings=%v", pl.ParseStatus, pl.ParseWarnings)
	}
}

// TestParse_AnnotationsUnaffectedByMarketDrift confirms that even
// when the entire event-slug payload is structurally broken, the
// annotations query (which lives in a sibling query) still parses.
// This is the fail-open guarantee the alert path depends on.
func TestParse_AnnotationsUnaffectedByMarketDrift(t *testing.T) {
	doc := `{
	  "pageProps":{
	    "dehydratedState":{
	      "queries":[
	        {"queryKey":["/api/event/slug","tx"],"state":{"data":"NOT_AN_OBJECT"}},
	        {"queryKey":["annotations","event","tx"],"state":{"data":[
	          {"timestamp":"2026-04-01T12:00:00Z","unixTime":1748707200,"title":"X","outcome":"Yes","priceBefore":0.5,"priceAfter":0.6,"priceChange":0.1}
	        ]}}
	      ]
	    }
	  }
	}`
	pl, err := parsePayload("tx", []byte(doc), time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pl.Annotations) != 1 {
		t.Errorf("annotations: got %d want 1", len(pl.Annotations))
	}
	if pl.ParseStatus != "partial" {
		t.Errorf("status: got %q want partial", pl.ParseStatus)
	}
	var sectionFail bool
	for _, w := range pl.ParseWarnings {
		if w.Section == "event" && w.Kind == "decode_failed" {
			sectionFail = true
		}
	}
	if !sectionFail {
		t.Errorf("expected decode_failed warning on event section; got %v", pl.ParseWarnings)
	}
}

// TestParse_FlexBool_DocumentsToleratedShape is a small contract
// test on the flex helpers themselves — protects against accidental
// regressions in the canonical/permissive branches.
func TestParse_FlexBool_DocumentsToleratedShape(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
		err  bool
	}{
		{`true`, true, false},
		{`false`, false, false},
		{`"true"`, true, false},
		{`"yes"`, true, false},
		{`"NO"`, false, false},
		{`""`, false, false},
		{`null`, false, false},
		{`"banana"`, false, true},
	} {
		var fb flexBool
		err := json.Unmarshal([]byte(tc.raw), &fb)
		if tc.err {
			if err == nil {
				t.Errorf("%s: want error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if fb.Bool() != tc.want {
			t.Errorf("%s: got %v want %v", tc.raw, fb.Bool(), tc.want)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}
