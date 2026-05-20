package alerting

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// NewsPredictionMarket is the compact market header the renderer needs.
type NewsPredictionMarket struct {
	Title             string
	Price             float64
	OneDayPriceChange *float64
	LifecyclePct      float64
	Volume24hUSD      float64
}

// NewsPredictionInputs is the full payload the Telegram renderer
// consumes. Every field is optional — empty sections elide.
type NewsPredictionInputs struct {
	Market            NewsPredictionMarket
	Prediction        repository.MarketPrediction
	Decision          marketprediction.Decision
	Blocked           *BlockedAlertView // (status, reason, scenarios) projection
	Repricing         *repricing.Signal
	Flow              *eventflow.EventFlowSummary
	AIText            string
	MatchedAlerts     []marketprediction.MatchedAlert
	LatestAnnotations []NewsPredictionAnnotation
}

// BlockedAlertView is the small projection used by the news/
// prediction renderer (kept distinct from anomaly.BlockedAlert to
// avoid importing anomaly here).
type BlockedAlertView struct {
	BlockedUntil         string
	Reason               string
	BullishScenario      string
	BearishScenario      string
	InvalidationScenario string
}

// NewsPredictionAnnotation is one annotation rendered at the bottom.
type NewsPredictionAnnotation struct {
	Date    string
	Title   string
	Outcome string
}

// RenderNewsPrediction emits the operator-facing News & Prediction
// HTML body. Sections elide when their inputs are empty. Telegram
// HTML escape applied at every boundary; nothing here trusts AI /
// Polymarket / operator strings as markup.
func RenderNewsPrediction(in NewsPredictionInputs) string {
	state := strings.TrimSpace(in.Decision.NewState)
	if state == "" {
		state = strings.TrimSpace(in.Prediction.CurrentState)
	}
	if state == "" {
		state = "watching"
	}
	title := strings.TrimSpace(in.Market.Title)
	if title == "" {
		title = "Polymarket event"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>NEWS &amp; PREDICTION: %s · %s</b>\n",
		html.EscapeString(state), html.EscapeString(title))

	// Market section.
	b.WriteString("\n<b>Market</b>\n")
	fmt.Fprintf(&b, "• price: %.4f\n", in.Market.Price)
	if in.Market.OneDayPriceChange != nil {
		fmt.Fprintf(&b, "• 24h: %+.3f\n", *in.Market.OneDayPriceChange)
	}
	if in.Market.LifecyclePct > 0 {
		fmt.Fprintf(&b, "• lifecycle: %.1f%%\n", in.Market.LifecyclePct)
	}
	if in.Market.Volume24hUSD > 0 {
		fmt.Fprintf(&b, "• volume24h: $%.0f\n", in.Market.Volume24hUSD)
	}

	// Prediction state section.
	if block := marketprediction.RenderTelegramBlock(in.Prediction, in.Decision); block != "" {
		b.WriteString("\n")
		b.WriteString(block)
	}

	// Blocked / catalyst section.
	if in.Blocked != nil {
		b.WriteString("\n<b>Blocked / Catalyst</b>\n")
		if in.Blocked.BlockedUntil != "" {
			fmt.Fprintf(&b, "• blocked until: %s\n", html.EscapeString(in.Blocked.BlockedUntil))
		}
		if in.Blocked.Reason != "" {
			fmt.Fprintf(&b, "• due to: %s\n", html.EscapeString(in.Blocked.Reason))
		}
		if in.Blocked.BullishScenario != "" {
			fmt.Fprintf(&b, "• bullish scenario: %s\n", html.EscapeString(in.Blocked.BullishScenario))
		}
		if in.Blocked.BearishScenario != "" {
			fmt.Fprintf(&b, "• bearish scenario: %s\n", html.EscapeString(in.Blocked.BearishScenario))
		}
		if in.Blocked.InvalidationScenario != "" {
			fmt.Fprintf(&b, "• invalidation: %s\n", html.EscapeString(in.Blocked.InvalidationScenario))
		}
	}

	// Repricing intelligence section.
	if in.Repricing != nil {
		b.WriteString("\n<b>Repricing intelligence</b>\n")
		fmt.Fprintf(&b, "• status: %s\n", html.EscapeString(in.Repricing.RepricingStatus))
		fmt.Fprintf(&b, "• flow timing: %s\n", html.EscapeString(in.Repricing.FlowTiming))
		if in.Repricing.PriceBefore != nil && in.Repricing.PriceAfter != nil {
			cur := "n/a"
			if in.Repricing.CurrentPrice != nil {
				cur = fmt.Sprintf("%.3f", *in.Repricing.CurrentPrice)
			}
			fmt.Fprintf(&b, "• price: before=%.3f → after=%.3f → current=%s\n",
				*in.Repricing.PriceBefore, *in.Repricing.PriceAfter, cur)
		}
		if in.Repricing.Explanation != "" {
			fmt.Fprintf(&b, "• interpretation: %s\n", html.EscapeString(in.Repricing.Explanation))
		}
	}

	// AI body — free-text Russian, HTML-escaped.
	if t := strings.TrimSpace(in.AIText); t != "" {
		b.WriteString("\n<b>AI prediction</b>\n")
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		b.WriteString(r.Replace(t))
		b.WriteString("\n")
	}

	// Matched alerts.
	if len(in.MatchedAlerts) > 0 {
		b.WriteString("\n<b>Matched Watchtower alerts</b>\n")
		const cap = 5
		n := len(in.MatchedAlerts)
		if n > cap {
			n = cap
		}
		for _, a := range in.MatchedAlerts[:n] {
			fmt.Fprintf(&b, "• %s · %s · score=%.2f · %s\n",
				html.EscapeString(a.Severity), html.EscapeString(a.Kind),
				a.Score, html.EscapeString(a.DirectionAlignment))
		}
	}

	// Latest Polymarket annotations.
	if len(in.LatestAnnotations) > 0 {
		b.WriteString("\n<b>Latest Polymarket events</b>\n")
		const cap = 5
		n := len(in.LatestAnnotations)
		if n > cap {
			n = cap
		}
		for i, a := range in.LatestAnnotations[:n] {
			fmt.Fprintf(&b, "%d. %s · %s",
				i+1, html.EscapeString(a.Date), html.EscapeString(a.Title))
			if a.Outcome != "" {
				fmt.Fprintf(&b, " · outcome=%s", html.EscapeString(a.Outcome))
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// AnnotationsForRender converts the eventflow / event-page rows into
// the renderer's compact projection.
func AnnotationsForRender(rows []repository.EventAnnotation, max int) []NewsPredictionAnnotation {
	out := make([]NewsPredictionAnnotation, 0, len(rows))
	for i, a := range rows {
		if i >= max {
			break
		}
		date := "—"
		if !a.Timestamp.IsZero() {
			date = a.Timestamp.UTC().Format("2006-01-02")
		}
		out = append(out, NewsPredictionAnnotation{
			Date: date, Title: a.Title, Outcome: a.Outcome,
		})
	}
	return out
}

// MatchedAlertsForRender adapts a slice into the renderer's
// expected order — Score desc.
func MatchedAlertsForRender(in []marketprediction.MatchedAlert) []marketprediction.MatchedAlert {
	out := append([]marketprediction.MatchedAlert(nil), in...)
	for i := range out {
		// Coerce timestamps to UTC so the renderer's output is
		// deterministic across timezones.
		out[i].AlertAt = out[i].AlertAt.UTC()
	}
	// Sort by score desc; stable to preserve insertion order on tie.
	stableSortByScore(out)
	return out
}

func stableSortByScore(s []marketprediction.MatchedAlert) {
	// Insertion sort — sample size is tiny (≤25).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Score > s[j-1].Score; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// _suppress unused-import linter complaints if a future test trims
// down the consumer surface.
var _ = time.Time{}
