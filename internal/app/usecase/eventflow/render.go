package eventflow

import (
	"fmt"
	"strings"
	"time"
)

// RenderPromptBlock formats the EventFlowSummary into a structured
// "Recent Watchtower flow:" prompt block. The text is structured
// `key: value` so the model can parse reliably. Empty summary
// emits the explicit "no meaningful stored flow" sentence the spec
// mandates — silence is never confused with weak flow.
func (s EventFlowSummary) RenderPromptBlock() string {
	var b strings.Builder
	b.WriteString("Recent Watchtower flow:\n")
	if s.Empty() {
		b.WriteString("No meaningful stored flow/anomaly data found for this event in the lookback window. ")
		b.WriteString("Do not infer weak flow from missing data.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "window: last %s\n", roundDur(s.Lookback))
	fmt.Fprintf(&b, "alerts: total=%d, info=%d, warning=%d, critical=%d, hard=%d\n",
		s.RecentAlerts, s.InfoAlerts, s.WarningAlerts, s.CriticalAlerts, s.HardAlerts)
	fmt.Fprintf(&b, "kinds: accumulation=%d, whale_flow=%d, cluster=%d, ownership=%d, stable_favorite=%d, new_wallet=%d\n",
		s.AccumulationAlerts, s.WhaleFlowAlerts, s.ClusterAlerts,
		s.OwnershipAlerts, s.StableFavoriteAlerts, s.NewWalletAlerts)
	if s.StrongestSide != "" {
		fmt.Fprintf(&b, "strongest side: %s on %s (condition=%s)\n",
			s.StrongestSide, oneLine(s.StrongestOutcome), s.StrongestConditionID)
	} else {
		b.WriteString("strongest side: unavailable\n")
	}
	if s.SameSideNotionalUSD > 0 || s.OppositeSideNotionalUSD > 0 {
		fmt.Fprintf(&b, "same-side notional: $%.0f\n", s.SameSideNotionalUSD)
		fmt.Fprintf(&b, "opposite-side notional: $%.0f\n", s.OppositeSideNotionalUSD)
		fmt.Fprintf(&b, "directional imbalance: %+.2f (net $%.0f)\n",
			s.DirectionalImbalance, s.NetDirectionalNotionalUSD)
	}
	if s.LargestTradeUSD > 0 {
		fmt.Fprintf(&b, "largest trade: $%.0f | outcome=%s | side=%s | wallet=%s | %s\n",
			s.LargestTradeUSD, oneLine(s.LargestTradeOutcome), s.LargestTradeSide,
			shortWallet(s.LargestTradeWallet), s.LargestTradeAt.UTC().Format(time.RFC3339))
	}
	for _, kv := range []struct{ label, val string }{
		{"accumulation", s.AccumulationNote},
		{"ownership", s.OwnershipNote},
		{"cluster", s.ClusterNote},
		{"stable favorite", s.StableFavoriteNote},
		{"whale flow", s.WhaleNote},
	} {
		if kv.val != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv.label, kv.val)
		}
	}
	if len(s.TopAlerts) > 0 {
		b.WriteString("top alerts:\n")
		for _, a := range s.TopAlerts {
			fmt.Fprintf(&b, "- %s · %s · %s · condition=%s · %s\n",
				a.Severity, a.Kind, oneLine(a.Question), a.ConditionID,
				a.CreatedAt.UTC().Format("2006-01-02T15:04Z"))
		}
	}
	if len(s.TopTrades) > 0 {
		b.WriteString("top trades:\n")
		for _, t := range s.TopTrades {
			fmt.Fprintf(&b, "- $%.0f | %s | outcome=%s | condition=%s | wallet=%s | %s\n",
				t.NotionalUSD, t.Side, oneLine(t.OutcomeToken), t.ConditionID,
				shortWallet(t.Wallet), t.TradedAt.UTC().Format("2006-01-02T15:04Z"))
		}
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

func shortWallet(w string) string {
	if len(w) < 10 {
		return w
	}
	return w[:6] + "…" + w[len(w)-4:]
}
