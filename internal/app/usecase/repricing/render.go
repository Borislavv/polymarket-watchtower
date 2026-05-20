package repricing

import (
	"fmt"
	"strings"
)

// RenderPromptBlock formats one Signal into the "Repricing
// intelligence" prompt slot. Empty / missing data renders an
// explicit "unavailable" sentence so the AI never confuses silence
// with stability.
func (s Signal) RenderPromptBlock() string {
	if s.EventSlug == "" && s.ConditionID == "" {
		return "Repricing intelligence: unavailable. No annotation-anchored repricing signal computed."
	}
	var b strings.Builder
	b.WriteString("Repricing intelligence:\n")
	if s.AnnotationTitle != "" {
		fmt.Fprintf(&b, "- annotation: %s\n", oneLine(s.AnnotationTitle))
	}
	if s.PriceBefore != nil && s.PriceAfter != nil {
		cur := "n/a"
		if s.CurrentPrice != nil {
			cur = fmt.Sprintf("%.3f", *s.CurrentPrice)
		}
		fmt.Fprintf(&b, "- price: before=%.3f -> after=%.3f -> current=%s\n",
			*s.PriceBefore, *s.PriceAfter, cur)
	}
	fmt.Fprintf(&b, "- repricing status: %s (confidence %.2f)\n", s.RepricingStatus, s.Confidence)
	fmt.Fprintf(&b, "- flow timing: %s\n", s.FlowTiming)
	if s.PreAnnotationFlowUSD > 0 {
		fmt.Fprintf(&b, "- pre-event flow: $%.0f\n", s.PreAnnotationFlowUSD)
	}
	if s.SameSidePostFlowUSD > 0 {
		fmt.Fprintf(&b, "- post-event same-side flow: $%.0f\n", s.SameSidePostFlowUSD)
	}
	if s.OppositeSidePostFlowUSD > 0 {
		fmt.Fprintf(&b, "- post-event opposite-side flow: $%.0f\n", s.OppositeSidePostFlowUSD)
	}
	if s.Explanation != "" {
		fmt.Fprintf(&b, "- interpretation: %s\n", oneLine(s.Explanation))
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}
