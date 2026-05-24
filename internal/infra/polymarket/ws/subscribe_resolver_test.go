package ws

import (
	"errors"
	"testing"
)

// TestResolveSubscribeTokens_KeepsEveryTokenForSelectedMarkets is the
// load-bearing v11.12-insider-prior invariant. A market with 8
// outcomes (May/Jul/Dec multi-leg shape) MUST produce 8 subscribed
// tokens. No silent truncation, no first-N slicing, no per-market
// cap.
func TestResolveSubscribeTokens_KeepsEveryTokenForSelectedMarkets(t *testing.T) {
	set := SubscriptionSet{Markets: []MarketSubscription{
		{
			ConditionID:  "0xmulti",
			CLOBTokenIDs: []string{"tok1", "tok2", "tok3", "tok4", "tok5", "tok6", "tok7", "tok8"},
		},
		{
			ConditionID:  "0xsingle",
			CLOBTokenIDs: []string{"tokA", "tokB"},
		},
	}}
	tokens, markets, err := resolveSubscribeTokens(set, 50_000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if markets != 2 {
		t.Fatalf("markets count: got %d want 2", markets)
	}
	if len(tokens) != 10 {
		t.Fatalf("must subscribe to all 10 tokens (8 multi + 2 single); got %d (%v)", len(tokens), tokens)
	}
}

// TestResolveSubscribeTokens_DedupsExactDuplicates proves we collapse
// identical token ids across markets (cheap defensive behaviour).
func TestResolveSubscribeTokens_DedupsExactDuplicates(t *testing.T) {
	set := SubscriptionSet{Markets: []MarketSubscription{
		{ConditionID: "0xA", CLOBTokenIDs: []string{"tok1", "tok2"}},
		{ConditionID: "0xB", CLOBTokenIDs: []string{"tok2", "tok3"}}, // tok2 dup
	}}
	tokens, _, err := resolveSubscribeTokens(set, 50_000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("must dedup to 3 distinct tokens; got %d (%v)", len(tokens), tokens)
	}
}

// TestResolveSubscribeTokens_OverHardCapFailsLoudly proves the
// v11.12-insider-prior circuit-breaker: when the resolved token
// count exceeds MaxTokensHardCap, the resolver returns
// ErrTokenHardCapExceeded LOUDLY rather than slicing the list.
func TestResolveSubscribeTokens_OverHardCapFailsLoudly(t *testing.T) {
	// 100 markets × 5 tokens = 500 tokens, cap 200 → must fail.
	set := SubscriptionSet{}
	for i := 0; i < 100; i++ {
		toks := []string{}
		for j := 0; j < 5; j++ {
			toks = append(toks, tokenName(i, j))
		}
		set.Markets = append(set.Markets, MarketSubscription{
			ConditionID:  conditionName(i),
			CLOBTokenIDs: toks,
		})
	}
	_, _, err := resolveSubscribeTokens(set, 200)
	if err == nil {
		t.Fatalf("expected ErrTokenHardCapExceeded; got nil")
	}
	if !errors.Is(err, ErrTokenHardCapExceeded) {
		t.Fatalf("expected ErrTokenHardCapExceeded; got %v", err)
	}
}

// TestResolveSubscribeTokens_DoesNotSilentlyCapAtMarketCount is the
// regression guard against the previous behaviour. Earlier the
// client capped tokens at MaxTokens, which silently truncated
// multi-outcome markets. New code only fails at the hard cap and
// otherwise keeps every token.
func TestResolveSubscribeTokens_DoesNotSilentlyCapAtMarketCount(t *testing.T) {
	set := SubscriptionSet{Markets: []MarketSubscription{
		{ConditionID: "0xA", CLOBTokenIDs: []string{"t1", "t2", "t3", "t4", "t5"}},
	}}
	// Hard cap = 5 EXACTLY; still passes.
	tokens, _, err := resolveSubscribeTokens(set, 5)
	if err != nil {
		t.Fatalf("at hard cap exactly: %v", err)
	}
	if len(tokens) != 5 {
		t.Fatalf("must keep all 5 tokens at exact hard cap; got %d", len(tokens))
	}
}

// TestResolveSubscribeTokens_EmptyTokenIDsAreSkipped proves we
// silently skip empty/whitespace token ids without aborting.
func TestResolveSubscribeTokens_EmptyTokenIDsAreSkipped(t *testing.T) {
	set := SubscriptionSet{Markets: []MarketSubscription{
		{ConditionID: "0xA", CLOBTokenIDs: []string{"t1", "", "  ", "t2"}},
	}}
	tokens, _, err := resolveSubscribeTokens(set, 50_000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(tokens) != 2 || tokens[0] != "t1" || tokens[1] != "t2" {
		t.Fatalf("must skip empty tokens; got %v", tokens)
	}
}

func tokenName(market, idx int) string { return "tok-" + intStr(market) + "-" + intStr(idx) }
func conditionName(i int) string       { return "0xcond-" + intStr(i) }
func intStr(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(digits[i%10]) + out
		i /= 10
	}
	return out
}
