package alerting

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// TelegramConfig wires a TelegramSink. It is kept here for backwards
// compatibility with the realtime fanout path (log + webhook). Production
// alert delivery in this release flows through the database queue and the
// internal/app/usecase/alertsender worker — that path uses telegram.Bot
// directly, without going through this sink.
type TelegramConfig struct {
	// Enabled turns the sink on. When false, Notify is a no-op.
	Enabled bool
	// BotToken is the bot's API token. Required when Enabled.
	BotToken string
	// ChatID is the single recipient (numeric chat id or @channelusername).
	// Required when Enabled.
	ChatID string
	// BaseURL defaults to https://api.telegram.org. Override for tests or a
	// corporate proxy.
	BaseURL string
	// Timeout for each send. Defaults to 5s.
	Timeout time.Duration
}

// TelegramSink delivers every Finding to a single Telegram chat as an HTML
// message. It is a thin adapter on top of internal/infra/telegram.Bot: this
// type owns the alerting-domain Channel interface and the per-severity
// metrics, the Bot owns the HTTP transport.
type TelegramSink struct {
	cfg     TelegramConfig
	bot     *telegram.Bot
	metrics *metrics.Metrics
}

// NewTelegramSink validates the config and returns a ready sink.
//
//   - Enabled=false → returns a no-op sink (Notify always returns nil).
//   - Enabled=true requires BotToken AND ChatID. Either missing is a startup
//     error — operators get an immediate signal, not a silent no-op.
func NewTelegramSink(cfg TelegramConfig) (*TelegramSink, error) {
	if !cfg.Enabled {
		return &TelegramSink{cfg: cfg}, nil
	}
	if cfg.BotToken == "" {
		return nil, errors.New("telegram: bot token required when enabled")
	}
	if cfg.ChatID == "" {
		return nil, errors.New("telegram: chat id required when enabled (TELEGRAM_CHAT_ID)")
	}
	bot, err := telegram.New(telegram.Config{
		BotToken: cfg.BotToken,
		BaseURL:  cfg.BaseURL,
		Timeout:  cfg.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("telegram sink: %w", err)
	}
	return &TelegramSink{cfg: cfg, bot: bot}, nil
}

// WithMetrics attaches a Prometheus metrics handle and returns the sink for
// chaining at construction time.
func (s *TelegramSink) WithMetrics(m *metrics.Metrics) *TelegramSink {
	s.metrics = m
	return s
}

// Name is the sink identifier used by the fanout for logging.
func (s *TelegramSink) Name() string { return "telegram" }

// Notify renders the finding and delivers it to the configured chat. When
// the sink is disabled this is a no-op (no error). Delivery errors are
// surfaced to the fanout for logging; the sink never retries.
func (s *TelegramSink) Notify(ctx context.Context, f anomaly.Finding) error {
	if !s.cfg.Enabled || s.bot == nil {
		return nil
	}
	text := FormatTelegramMessage(f)
	if _, err := s.bot.SendHTML(ctx, s.cfg.ChatID, text); err != nil {
		s.observeErr(f.Severity)
		return err
	}
	s.observeOK(f.Severity)
	return nil
}

func (s *TelegramSink) observeOK(sev anomaly.Severity) {
	if s.metrics != nil {
		s.metrics.TelegramAlertsSent.WithLabelValues(string(sev)).Inc()
	}
}

func (s *TelegramSink) observeErr(sev anomaly.Severity) {
	if s.metrics != nil {
		s.metrics.TelegramAlertErrors.WithLabelValues(string(sev)).Inc()
	}
}

// FormatTelegramMessage renders the HTML body for one finding. The output is
// structured into clearly-separated sections so a human glancing at the alert
// on a phone gets the answer fast: severity headline → why → trade → cluster
// → links. Exposed so tests can assert on it without invoking HTTP.
//
// HTML parse mode requires escaping `&`, `<`, `>`. We use html.EscapeString
// for any user-supplied content (market titles, wallet, etc.) — no homemade
// escaping.
func FormatTelegramMessage(f anomaly.Finding) string {
	switch f.Kind {
	case anomaly.KindCategoryWatch:
		return formatCategoryWatch(f)
	case anomaly.KindAccumulation:
		return formatAccumulation(f)
	default:
		return formatTradeAnomaly(f)
	}
}

// formatAccumulation renders the same-trader accumulation-line alert
// payload. Distinct shape from single-trade and cluster: the header
// surfaces line count + total notional, and the body explains what the
// wallet has been doing (median trade, max trade, span, axis multipliers,
// score / confidence). Reason codes are listed verbatim so operators can
// correlate with the metric labels.
func formatAccumulation(f anomaly.Finding) string {
	var b strings.Builder
	writeAccumulationHeader(&b, f)
	writeAccumulationWhy(&b, f)
	writeAccumulationTraderLine(&b, f)
	writeLinks(&b, f)
	return b.String()
}

func writeAccumulationHeader(b *strings.Builder, f anomaly.Finding) {
	title := tradeTitle(f)
	hot := ""
	if f.Hot {
		hot = " · HOT"
	}
	total, count := 0.0, 0
	if f.Accumulation != nil {
		total = f.Accumulation.TotalNotionalUSD
		count = f.Accumulation.TradeCount
	}
	fmt.Fprintf(b, "<b>%s: accumulation · $%s across %d trades%s · %s</b>\n",
		strings.ToUpper(string(f.Severity)),
		money(total),
		count,
		hot,
		html.EscapeString(title),
	)
}

func writeAccumulationWhy(b *strings.Builder, f anomaly.Finding) {
	a := f.Accumulation
	if a == nil {
		return
	}
	b.WriteString("\n<b>Why</b>\n")
	b.WriteString("• same wallet accumulated one outcome\n")
	fmt.Fprintf(b, "• total line: <b>$%s</b> across <b>%d</b> trades\n", money(a.TotalNotionalUSD), a.TradeCount)
	fmt.Fprintf(b, "• avg trade: $%s, median: $%s, max trade: $%s\n",
		money(a.MeanNotionalUSD), money(a.MedianNotionalUSD), money(a.MaxNotionalUSD))
	if a.AvgOdds > 0 {
		fmt.Fprintf(b, "• avg odds: <b>x%s</b>, max odds: x%s\n",
			multiplierFmt(a.AvgOdds), multiplierFmt(a.MaxOdds))
	}
	if a.MarketMultiplier > 0 {
		fmt.Fprintf(b, "• line is <b>%sx</b> above market baseline median\n", multiplierFmt(a.MarketMultiplier))
	}
	if a.TraderMultiplier > 0 {
		fmt.Fprintf(b, "• line is <b>%sx</b> above wallet's typical trade\n", multiplierFmt(a.TraderMultiplier))
	}
	if f.LifecyclePct > 0 {
		hot := ""
		if f.Hot {
			hot = " (HOT — final stretch)"
		}
		fmt.Fprintf(b, "• market lifecycle: <b>%.1f%%</b> elapsed%s\n", f.LifecyclePct, hot)
	}
	fmt.Fprintf(b, "• score: <b>%d/100</b>, confidence: <b>%.2f</b>, size path: <code>%s</code>\n",
		a.Score, a.Confidence, nonEmptyOr(a.SizePath, "n/a"))
	if len(a.Reasons) > 0 {
		fmt.Fprintf(b, "• reasons: <code>%s</code>\n", html.EscapeString(strings.Join(a.Reasons, ", ")))
	}
}

func writeAccumulationTraderLine(b *strings.Builder, f anomaly.Finding) {
	a := f.Accumulation
	if a == nil {
		return
	}
	b.WriteString("\n<b>Trader line</b>\n")
	fmt.Fprintf(b, "• wallet: <code>%s</code>\n", html.EscapeString(a.Wallet))
	fmt.Fprintf(b, "• outcome: <b>%s</b> (%s)\n", html.EscapeString(a.Outcome), html.EscapeString(a.Side))
	fmt.Fprintf(b, "• trades in line: <b>%d</b>\n", a.TradeCount)
	fmt.Fprintf(b, "• span: %s\n", humanDuration(a.Span))
	// Same-side ratio is 1.0 by construction (the query filters by side).
	// We surface it explicitly so an operator reviewing a near-miss in a
	// future debug log sees the figure.
	b.WriteString("• same-side ratio: 100%\n")
}

func formatTradeAnomaly(f anomaly.Finding) string {
	var b strings.Builder
	writeTradeHeader(&b, f)
	writeWhy(&b, f)
	writeTrade(&b, f)
	writeLinks(&b, f)
	return b.String()
}

func formatCategoryWatch(f anomaly.Finding) string {
	var b strings.Builder
	writeClusterHeader(&b, f)
	writeCluster(&b, f)
	writeLinks(&b, f)
	return b.String()
}

// --- HTML section builders --------------------------------------------------

func writeTradeHeader(b *strings.Builder, f anomaly.Finding) {
	title := tradeTitle(f)
	hot := ""
	if f.Hot {
		hot = " · HOT"
	}
	fmt.Fprintf(b, "<b>%s: x%s · $%s%s · %s</b>\n",
		strings.ToUpper(string(f.Severity)),
		multiplierFmt(f.EffectiveMultiplier),
		money(notional(f)),
		hot,
		html.EscapeString(title),
	)
}

func writeClusterHeader(b *strings.Builder, f anomaly.Finding) {
	cat := "(uncategorised)"
	if f.Category != nil && f.Category.Label != "" {
		cat = f.Category.Label
	}
	totalUSD, count, wallets := 0.0, 0, 0
	if f.Cluster != nil {
		totalUSD = f.Cluster.TotalUSD
		count = f.Cluster.AnomalousTrades
		wallets = f.Cluster.UniqueWallets
	}
	hot := ""
	if f.Hot {
		hot = " · HOT"
	}
	fmt.Fprintf(b, "<b>%s — CategoryWatchRequired: %d trades · %d wallets · $%s%s · %s</b>\n",
		strings.ToUpper(string(f.Severity)), count, wallets, money(totalUSD), hot, html.EscapeString(cat))
}

func writeWhy(b *strings.Builder, f anomaly.Finding) {
	b.WriteString("\n<b>Why</b>\n")
	if f.EffectiveMultiplier > 0 {
		axis := f.MultiplierAxis
		switch axis {
		case "market":
			if f.Baseline != nil {
				fmt.Fprintf(b, "• <b>x%s</b> above market baseline median ($%s)\n",
					multiplierFmt(f.MarketMultiplier), money(f.Baseline.MedianUSD))
			}
		case "trader":
			if f.TraderBaseline != nil {
				fmt.Fprintf(b, "• <b>x%s</b> above wallet's typical trade ($%s median)\n",
					multiplierFmt(f.TraderMultiplier), money(f.TraderBaseline.MedianUSD))
			}
		case "both":
			fmt.Fprintf(b, "• <b>x%s</b> above both market and wallet medians (rare on both axes)\n",
				multiplierFmt(f.EffectiveMultiplier))
		default:
			fmt.Fprintf(b, "• <b>x%s</b> above baseline median\n", multiplierFmt(f.EffectiveMultiplier))
		}
	}
	if f.Trade != nil && f.Trade.Odds > 0 {
		fmt.Fprintf(b, "• odds <b>%s</b>, implied probability <b>%.1f%%</b>\n",
			multiplierFmt(f.Trade.Odds), f.Trade.Price*100)
	}
	if f.Baseline != nil {
		fmt.Fprintf(b, "• market baseline: <b>%d</b> trades, median $%s, mean $%s, p95 $%s, span %s\n",
			f.Baseline.SampleN, money(f.Baseline.MedianUSD), money(f.Baseline.MeanUSD), money(f.Baseline.P95USD),
			humanDuration(f.Baseline.Span))
	}
	if f.TraderBaseline != nil {
		fmt.Fprintf(b, "• trader history: <b>%d</b> trades, median $%s, p95 $%s, span %s\n",
			f.TraderBaseline.SampleN, money(f.TraderBaseline.MedianUSD), money(f.TraderBaseline.P95USD),
			humanDuration(f.TraderBaseline.Span))
	}
	if f.AbsoluteTier != "" || f.MultiplierTier != "" {
		fmt.Fprintf(b, "• tiers: absolute=<code>%s</code> multiplier=<code>%s</code> (axis=%s) → final=<b>%s</b>\n",
			string(f.AbsoluteTier), string(f.MultiplierTier), nonEmptyOr(f.MultiplierAxis, "n/a"), string(f.Severity))
	}
	if f.LifecyclePct > 0 {
		hot := ""
		if f.Hot {
			hot = " (HOT — final stretch)"
		}
		fmt.Fprintf(b, "• market lifecycle: <b>%.1f%%</b> elapsed%s\n", f.LifecyclePct, hot)
	}
	switch {
	case f.Kind == anomaly.KindTradeAnomaly && f.InCluster:
		fmt.Fprintf(b, "• <b>part of a forming cluster</b>: %d anomalous trades in the current window\n", f.ClusterPeerCount)
	case f.Kind == anomaly.KindTradeAnomaly:
		b.WriteString("• single trade (no peers in cluster window yet)\n")
	}
}

func nonEmptyOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func writeTrade(b *strings.Builder, f anomaly.Finding) {
	if f.Trade == nil {
		return
	}
	t := f.Trade
	b.WriteString("\n<b>Trade</b>\n")
	if t.Outcome != "" || t.Side != "" {
		fmt.Fprintf(b, "• outcome: <b>%s</b> (%s)\n",
			html.EscapeString(t.Outcome), html.EscapeString(string(t.Side)))
	}
	fmt.Fprintf(b, "• size: $%s (%.2f shares @ %.4f)\n", money(t.NotionalUSD), t.SizeShares, t.Price)
	if t.Wallet != "" {
		fmt.Fprintf(b, "• trader: <code>%s</code>\n", html.EscapeString(t.Wallet))
	}
	if f.Category != nil && f.Category.Label != "" {
		fmt.Fprintf(b, "• category: %s\n", html.EscapeString(f.Category.Label))
	}
	fmt.Fprintf(b, "• time: %s\n", t.At.UTC().Format("2006-01-02 15:04:05 UTC"))
}

func writeCluster(b *strings.Builder, f anomaly.Finding) {
	if f.Cluster == nil {
		return
	}
	c := f.Cluster
	b.WriteString("\n<b>Cluster</b>\n")
	fmt.Fprintf(b, "• <b>%d anomalous trades</b>\n", c.AnomalousTrades)
	fmt.Fprintf(b, "• <b>%d unique traders</b>\n", c.UniqueWallets)
	fmt.Fprintf(b, "• <b>$%s total anomalous notional</b>\n", money(c.TotalUSD))
	fmt.Fprintf(b, "• window: %s\n", c.Window)
	if len(c.Sample) > 0 {
		b.WriteString("\n<b>Recent contributors</b>\n")
		for _, t := range c.Sample {
			title := t.Question
			if title == "" {
				title = t.Slug
			}
			if title == "" {
				title = string(t.Market)
			}
			fmt.Fprintf(b, "• $%s on <i>%s</i> — <code>%s</code> %s\n",
				money(t.NotionalUSD), html.EscapeString(title),
				html.EscapeString(shortWallet(t.Wallet)), html.EscapeString(t.Outcome))
		}
	}
}

// writeLinks renders the bulleted Links section. Every entry is a Telegram
// HTML <a href> anchor produced by renderLink — entries whose href is empty
// are omitted entirely (no plain-text label is ever emitted). The whole
// section is skipped when nothing is renderable.
func writeLinks(b *strings.Builder, f anomaly.Finding) {
	entries := []struct{ label, href string }{
		{"Polymarket market", f.MarketURL},
		{"Polymarket category", f.CategoryURL},
		{"Trader", f.TraderURL},
		{"Grafana", f.GrafanaURL},
	}
	any := false
	for _, e := range entries {
		if e.href != "" {
			any = true
			break
		}
	}
	if !any {
		return
	}
	b.WriteString("\n<b>Links</b>\n")
	for _, e := range entries {
		if e.href == "" {
			continue
		}
		b.WriteString("• ")
		b.WriteString(renderLink(e.label, e.href))
		b.WriteByte('\n')
	}
}

// renderLink returns a Telegram HTML-parse-mode anchor: `<a href="...">label</a>`.
//
// Both inputs are HTML-escaped via html.EscapeString so Telegram parses the
// tag instead of treating loose '&' or '<' as literal text. We deliberately
// do NOT URL-encode the href — callers must hand in a fully-encoded URL
// (i.e. one built via net/url). Empty href returns the literal label
// unwrapped, but callers in this package skip empty hrefs upstream so this
// path exists only as a safety net for future reuse.
func renderLink(label, href string) string {
	if href == "" {
		return html.EscapeString(label)
	}
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + `</a>`
}

// --- helpers ---------------------------------------------------------------

func tradeTitle(f anomaly.Finding) string {
	if f.Trade != nil {
		if f.Trade.Question != "" {
			return f.Trade.Question
		}
		if f.Trade.Slug != "" {
			return f.Trade.Slug
		}
		return string(f.Trade.Market)
	}
	return "anomaly"
}

func notional(f anomaly.Finding) float64 {
	if f.Trade != nil {
		return f.Trade.NotionalUSD
	}
	if f.Cluster != nil {
		return f.Cluster.TotalUSD
	}
	return 0
}

// shortWallet returns "0xabcd…wxyz" for a 0x-address.
func shortWallet(w string) string {
	if len(w) < 12 {
		return w
	}
	return w[:6] + "…" + w[len(w)-4:]
}

// money formats a USD amount with thousand separators and 0/2 decimals.
func money(v float64) string {
	if v >= 1000 {
		whole := int64(v)
		s := fmt.Sprintf("%d", whole)
		n := len(s)
		out := make([]byte, 0, n+n/3)
		for i, c := range []byte(s) {
			if i > 0 && (n-i)%3 == 0 {
				out = append(out, ',')
			}
			out = append(out, c)
		}
		return string(out)
	}
	return fmt.Sprintf("%.2f", v)
}

// humanDuration prints durations in operator-friendly units. Sub-minute is
// rendered as "&lt;1m" — the literal `<1m` would be interpreted by
// Telegram's HTML parser as the start of an unknown tag named "1m" and
// the whole message would be rejected with a 400 from the Bot API. Using
// the HTML entity keeps the visual ("<1m") while staying parseable.
//
// Regression: TestHumanDurationIsHTMLSafe + TestFormatTelegramMessageNeverEmitsRawLT.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "&lt;1m"
	}
	const day = 24 * time.Hour
	days := int(d / day)
	hours := int((d % day) / time.Hour)
	switch {
	case days >= 1 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days >= 1:
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	default:
		m := int(d / time.Minute)
		if m == 0 {
			return "&lt;1m"
		}
		return fmt.Sprintf("%dm", m)
	}
}

func multiplierFmt(m float64) string {
	switch {
	case m >= 100:
		return fmt.Sprintf("%.0f", m)
	case m >= 10:
		return fmt.Sprintf("%.1f", m)
	default:
		return fmt.Sprintf("%.2f", m)
	}
}
