package newsintel

import (
	"fmt"
	"html"
	"strings"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
)

// polymarketBase is the public host for market + profile links.
// Hard-coded — the worker has no event-page slug indirection (the
// alertsender already handles 307 redirects via the slug-alias
// table; that surface is irrelevant here because we only render
// canonical /event/<slug> paths).
const polymarketBase = "https://polymarket.com"

// RenderTelegramMessage renders the v11.0 news intel Telegram body.
// All operator-visible strings — including AI-authored fields and
// Polymarket-authored titles — are HTML-escaped at the boundary.
//
// Sections (in order, each elided when empty):
//
//	<b>📰 News intel · DECISION</b>
//	<i>summary</i>
//
//	1. <b>YES_up · x2h · conf 0.78</b> — Trump endorses NY mayor
//	   <i>Market: …</i>
//	   Why it matters: …
//	   What market may miss: …
//	   Trigger: …
//	   Invalidates if: …
//	   Stance: consider
//	   <a href="…">market</a> · <a href="…">event annotations</a>
//
//	2. …
func RenderTelegramMessage(
	result openai.NewsIntelAIResult,
	selected []openai.NewsIntelAIDecision,
	affected map[string][]openai.NewsAffectedMarketForAI,
) string {
	var b strings.Builder

	decision := strings.ToUpper(strings.TrimSpace(result.Decision))
	if decision == "" {
		decision = "WATCH"
	}
	fmt.Fprintf(&b, "<b>📰 News intel · %s</b>\n", html.EscapeString(decision))
	if s := strings.TrimSpace(result.Summary); s != "" {
		fmt.Fprintf(&b, "<i>%s</i>\n", html.EscapeString(s))
	}

	for i, d := range selected {
		b.WriteString("\n")
		head := strings.Builder{}
		impact := strings.TrimSpace(d.ImpactDirection)
		if impact == "" {
			impact = "unclear"
		}
		win := strings.TrimSpace(d.ExpectedWindow)
		if win == "" {
			win = "unclear"
		}
		fmt.Fprintf(&head, "%d. <b>%s · %s · conf %.2f</b>",
			i+1,
			html.EscapeString(impact),
			html.EscapeString(win),
			d.Confidence,
		)
		b.WriteString(head.String())
		b.WriteString("\n")
		if t := strings.TrimSpace(d.MarketTitle); t != "" {
			fmt.Fprintf(&b, "<i>%s</i>\n", html.EscapeString(t))
		}
		writeKV(&b, "Why it matters", d.WhyItMatters)
		writeKV(&b, "What market may miss", d.WhatMarketMayMiss)
		writeKV(&b, "Trigger", d.TriggerCondition)
		writeKV(&b, "Invalidates if", d.InvalidatesIf)
		if stance := strings.TrimSpace(d.TradeStance); stance != "" {
			fmt.Fprintf(&b, "Stance: <b>%s</b>\n", html.EscapeString(stance))
		}
		links := renderLinks(d, affected)
		if links != "" {
			b.WriteString(links)
			b.WriteString("\n")
		}
		writeAffectedBlock(&b, d, affected)
	}

	return strings.TrimRight(b.String(), "\n")
}

func writeKV(b *strings.Builder, label, value string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	fmt.Fprintf(b, "%s: %s\n", html.EscapeString(label), html.EscapeString(v))
}

// renderLinks emits the primary "Polymarket market" + "Trader profile"
// anchors when the decision carries enough context. Only http(s)
// links are emitted; the event_slug must be present.
func renderLinks(d openai.NewsIntelAIDecision, _ map[string][]openai.NewsAffectedMarketForAI) string {
	slug := strings.TrimSpace(d.EventSlug)
	if slug == "" {
		return ""
	}
	link := fmt.Sprintf("%s/event/%s", polymarketBase, html.EscapeString(slug))
	return fmt.Sprintf(`<a href="%s">market</a>`, link)
}

// writeAffectedBlock surfaces additional affected markets BELOW the
// AI-chosen primary market so the operator sees the full blast
// radius. Elided when the news item has only one affected market
// (the one the AI already picked).
func writeAffectedBlock(b *strings.Builder, d openai.NewsIntelAIDecision, affected map[string][]openai.NewsAffectedMarketForAI) {
	list := affected[d.NewsItemHash]
	if len(list) <= 1 {
		return
	}
	siblings := make([]openai.NewsAffectedMarketForAI, 0, len(list))
	for _, m := range list {
		if m.ConditionID == d.ConditionID {
			continue
		}
		siblings = append(siblings, m)
	}
	if len(siblings) == 0 {
		return
	}
	b.WriteString("Affected markets:\n")
	for _, m := range siblings {
		title := strings.TrimSpace(m.MarketTitle)
		if title == "" {
			title = m.ConditionID
		}
		fmt.Fprintf(b, " · %s\n", html.EscapeString(title))
	}
}
