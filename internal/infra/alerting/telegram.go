package alerting

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/url"
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

// TelegramSink is the dev-mode synchronous fanout sink. Production
// alert delivery flows through the DB queue → alertsender worker →
// *telegram.Router. This sink only matters when Postgres is
// unwired (developer laptop / tests).
//
// v11.4: the sink now dispatches through a typed telegram.Sender
// (the Router) instead of touching *Bot directly. The legacy
// constructor that takes a token + chat id is retained so the
// "Enabled=false" no-op behaviour is preserved, but every
// outbound message carries an explicit Surface so the router can
// route, log, and metric-attribute it.
type TelegramSink struct {
	cfg     TelegramConfig
	sender  telegram.Sender
	metrics *metrics.Metrics
}

// NewTelegramSink validates the config and returns a disabled sink
// when Enabled=false. The legacy bot-token / chat-id fields are
// kept on the config struct for backward compatibility but are no
// longer used to construct a transport — callers MUST pass a typed
// Sender via WithSender().
//
// Practical wiring: in dev mode the app boots without Postgres and
// the realtime fanout calls NewTelegramSink(...).WithSender(router).
// In production the alertsender path is used instead and this sink
// is left disabled.
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
	return &TelegramSink{cfg: cfg}, nil
}

// WithSender attaches the typed Telegram Sender. Production passes
// the *telegram.Router; tests pass a recording fake. Without a
// Sender the enabled sink is inert (Notify returns nil).
func (s *TelegramSink) WithSender(sender telegram.Sender) *TelegramSink {
	s.sender = sender
	return s
}

// WithMetrics attaches a Prometheus metrics handle and returns the sink for
// chaining at construction time.
func (s *TelegramSink) WithMetrics(m *metrics.Metrics) *TelegramSink {
	s.metrics = m
	return s
}

// Name is the sink identifier used by the fanout for logging.
func (s *TelegramSink) Name() string { return "telegram" }

// Notify renders the finding and dispatches it through the typed
// Sender with a Finding.Kind-derived Surface. When the sink is
// disabled OR no Sender has been attached, this is a no-op
// (no error). Delivery errors propagate to the fanout for logging;
// the sink never retries.
//
// v11.4: the chat id is resolved by the Router from the Surface —
// the sink no longer carries a static ChatID through to the
// transport. This guarantees a flow alert can never accidentally
// land on an admin chat even if cfg.ChatID is misconfigured.
func (s *TelegramSink) Notify(ctx context.Context, f anomaly.Finding) error {
	if !s.cfg.Enabled || s.sender == nil {
		return nil
	}
	text := FormatTelegramMessage(f)
	if _, err := s.sender.Send(ctx, telegram.Message{
		Surface: SurfaceForFindingKind(f.Kind),
		HTML:    text,
	}); err != nil {
		s.observeErr(f.Severity)
		return err
	}
	s.observeOK(f.Severity)
	return nil
}

// SurfaceForFindingKind maps a Finding.Kind onto the typed Telegram
// surface used by the v11.3 Router. Mirrors the alertsender helper
// so the two send paths assign the same surface label to identical
// alert types.
//
// Exported for the dev-mode fanout wiring + tests that need to
// assert which surface a Finding produces.
func SurfaceForFindingKind(kind anomaly.Kind) telegram.Surface {
	switch kind {
	case anomaly.KindTradeAnomaly:
		return telegram.SurfaceFlowAlert
	case anomaly.KindAccumulation:
		return telegram.SurfaceAccumulationAlert
	case anomaly.KindCategoryWatch:
		return telegram.SurfaceClusterAlert
	case anomaly.KindOwnership:
		return telegram.SurfaceOwnershipAlert
	}
	return telegram.SurfaceFlowAlert
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
	case anomaly.KindOwnership:
		return formatOwnership(f)
	case anomaly.KindStableFavorite:
		return formatStableFavorite(f)
	default:
		return formatTradeAnomaly(f)
	}
}

// formatStableFavorite renders the late-market-stable-favorite alert.
// Deliberately distinct from whale-flow shapes: the header carries
// probability + return %, the Why block surfaces stability stats,
// and the Risks block is REQUIRED — never describe these as safe
// or guaranteed.
func formatStableFavorite(f anomaly.Finding) string {
	var b strings.Builder
	sf := f.StableFavorite
	if sf == nil {
		// Defensive — should never happen, but render a minimal
		// envelope instead of panicking.
		fmt.Fprintf(&b, "<b>%s · stable favorite</b>\n", strings.ToUpper(string(f.Severity)))
		writeLinks(&b, f)
		writeAnalystNote(&b, f)
		return b.String()
	}
	hot := ""
	if f.Hot {
		hot = " · HOT"
	}
	title := tradeTitle(f) // falls back to "Polymarket market" when Trade is nil
	fmt.Fprintf(&b, "<b>%s · stable favorite · %.0f%% · +%.0f%% return%s · %s</b>\n",
		strings.ToUpper(string(f.Severity)),
		sf.Probability*100,
		sf.RemainingReturnPct,
		hot,
		html.EscapeString(title),
	)

	b.WriteString("\n<b>Why</b>\n")
	fmt.Fprintf(&b, "• market is <b>%.1f%%</b> through lifecycle\n", sf.LifecyclePct)
	if sf.PriceSamples >= 2 {
		fmt.Fprintf(&b, "• favorite price stable: %.0f–%.0f%% over %s (stddev %.3f)\n",
			sf.PriceMin*100, sf.PriceMax*100, humanDuration(sf.StabilityWindow), sf.PriceStddev)
	}
	fmt.Fprintf(&b, "• remaining return if correct: <b>+%.1f%%</b>\n", sf.RemainingReturnPct)
	if sf.AdverseMove6h < 0 {
		fmt.Fprintf(&b, "• adverse 6h drift: %+.1f%% (above the gate's tolerance is rejected upstream)\n", sf.AdverseMove6h*100)
	} else {
		b.WriteString("• no adverse drift in last 6h\n")
	}
	fmt.Fprintf(&b, "• liquidity: $%s volume, %d recent trades\n", money(sf.RecentVolumeUSD), sf.RecentTradeCount)
	fmt.Fprintf(&b, "• confidence: <b>%.2f</b> · score: <b>%.0f</b>/100\n", sf.Confidence, sf.Score)

	b.WriteString("\n<b>Risks</b>\n")
	b.WriteString("• political markets can gap on late news — this is NOT a guarantee\n")
	switch sf.CrossMarketStatus {
	case "confirmed":
		fmt.Fprintf(&b, "• cross-market check: <b>confirmed</b> (within %.2f price units)\n", sf.CrossMarketDelta)
	case "conflict":
		fmt.Fprintf(&b, "• cross-market check: <b>conflict</b> (Δ %.2f) — treat as low confidence\n", sf.CrossMarketDelta)
	default:
		b.WriteString("• cross-market check: <b>unavailable</b> — no related-market price wired\n")
	}
	if sf.RecentVolumeUSD < 50_000 {
		b.WriteString("• thin liquidity may slow exit\n")
	}

	writeLinks(&b, f)
	writeAnalystNote(&b, f)
	return b.String()
}

// formatOwnership renders the Strategy-E market-ownership concentration
// alert. The body is visually distinct from the other kinds: header
// carries the share percentage, the Why block surfaces the approximation
// caveat, and the Data block carries dedup + market_id + outcome_token.
//
// The percentage rendering is deliberately blunt about the approximation —
// no holders endpoint is wired upstream, so the figure is computed from
// trade-flow share counts only. An operator must read "17.4% of recorded
// flow" not "17.4% of the market".
func formatOwnership(f anomaly.Finding) string {
	var b strings.Builder
	writeOwnershipHeader(&b, f)
	writeOwnershipWhy(&b, f)
	writeLinks(&b, f)
	writeAnalystNote(&b, f)
	return b.String()
}

func writeOwnershipHeader(b *strings.Builder, f anomaly.Finding) {
	pct := 0.0
	if f.Ownership != nil {
		pct = f.Ownership.SharePct
	}
	title := tradeTitle(f)
	fmt.Fprintf(b, "<b>%s: ownership concentration · %.1f%% · %s</b>\n",
		strings.ToUpper(string(f.Severity)), pct, html.EscapeString(title))
}

func writeOwnershipWhy(b *strings.Builder, f anomaly.Finding) {
	if f.Ownership == nil {
		return
	}
	o := f.Ownership
	b.WriteString("\n<b>Why</b>\n")
	fmt.Fprintf(b, "• wallet owns <b>%.1f%%</b> of recorded BUY-side flow on this outcome\n", o.SharePct)
	if o.NotionalUSD > 0 {
		fmt.Fprintf(b, "• position value estimate: <b>$%s</b> (net %s shares × last price)\n",
			money(o.NotionalUSD), formatShareCount(o.WalletNetShares))
	}
	if o.MarketTotalShares > 0 {
		fmt.Fprintf(b, "• market total recorded BUY shares: %s\n", formatShareCount(o.MarketTotalShares))
	}
	if o.Outcome != "" {
		fmt.Fprintf(b, "• outcome: <b>%s</b>\n", html.EscapeString(o.Outcome))
	}
	if o.Approximate {
		b.WriteString("• <i>trade-flow approximation</i> — no holders endpoint wired; figure is directional, not authoritative\n")
	}
	for _, r := range f.Reasons {
		if r == "" {
			continue
		}
		fmt.Fprintf(b, "• reason: <code>%s</code>\n", html.EscapeString(r))
	}
}

// formatShareCount renders a share count with thousand separators (no
// decimals — fractional shares are rare on Polymarket and the operator
// doesn't read them). 0 → "0".
func formatShareCount(v float64) string {
	if v < 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	return money(v)
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
	writeAnalystNote(&b, f)
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
	if a.Window != "" {
		fmt.Fprintf(b, "• window: <b>%s</b>\n", html.EscapeString(a.Window))
	}
	writeQuietMarket(b, f)
	writeNewWallet(b, f)
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
	writeAnalystNote(&b, f)
	return b.String()
}

func formatCategoryWatch(f anomaly.Finding) string {
	var b strings.Builder
	writeClusterHeader(&b, f)
	writeCluster(&b, f)
	writeLinks(&b, f)
	writeAnalystNote(&b, f)
	return b.String()
}

// --- HTML section builders --------------------------------------------------

func writeTradeHeader(b *strings.Builder, f anomaly.Finding) {
	title := tradeTitle(f)
	hot := ""
	if f.Hot {
		hot = " · HOT"
	}
	// v6 header: severity, optional profit-if-win, notional, optional
	// HOT, title. The profit segment is OMITTED rather than printed as
	// "$0.00" when the payload doesn't carry a payoff value — typically
	// a legacy alert serialized by a pre-v6 binary, or an input that
	// failed validation (odds ≤ 1).
	profit := ""
	if f.ProfitIfWinUSD > 0 {
		profit = "profit $" + money(f.ProfitIfWinUSD) + " · "
	}
	fmt.Fprintf(b, "<b>%s: %s$%s%s · %s</b>\n",
		strings.ToUpper(string(f.Severity)),
		profit,
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
	// Payoff first — the operator-relevant magnitude.
	if f.ProfitIfWinUSD > 0 {
		fmt.Fprintf(b, "• payoff if win: profit <b>$%s</b> (gross $%s)\n",
			money(f.ProfitIfWinUSD), money(f.GrossPayoutIfWinUSD))
	}
	// Market tail row — only when the market baseline was ready.
	if f.Baseline != nil && f.Baseline.SampleN > 0 && (f.MarketP95Ratio > 0 || f.MarketP99Ratio > 0) {
		writeTailRow(b, "market tail", notional(f), f.MarketP95Ratio, f.Baseline.P95USD, f.MarketP99Ratio, f.Baseline.P99USD)
	}
	// Trader tail row — only when the trader baseline was ready.
	if f.TraderBaseline != nil && f.TraderBaseline.SampleN > 0 && (f.TraderP95Ratio > 0 || f.TraderP99Ratio > 0) {
		writeTailRow(b, "trader tail", notional(f), f.TraderP95Ratio, f.TraderBaseline.P95USD, f.TraderP99Ratio, f.TraderBaseline.P99USD)
	}
	if f.Trade != nil && f.Trade.Odds > 0 {
		fmt.Fprintf(b, "• odds <b>%s</b>, implied probability <b>%.1f%%</b>\n",
			multiplierFmt(f.Trade.Odds), f.Trade.Price*100)
	}
	if f.Baseline != nil {
		fmt.Fprintf(b, "• market baseline: <b>%d</b> trades, median $%s, mean $%s, p95 %s, p99 %s, span %s\n",
			f.Baseline.SampleN, money(f.Baseline.MedianUSD), money(f.Baseline.MeanUSD),
			percentileCell(f.Baseline.P95USD), percentileCell(f.Baseline.P99USD),
			humanDuration(f.Baseline.Span))
	}
	if f.TraderBaseline != nil {
		fmt.Fprintf(b, "• trader history: <b>%d</b> trades, median $%s, p95 %s, p99 %s, span %s\n",
			f.TraderBaseline.SampleN, money(f.TraderBaseline.MedianUSD),
			percentileCell(f.TraderBaseline.P95USD), percentileCell(f.TraderBaseline.P99USD),
			humanDuration(f.TraderBaseline.Span))
	}
	if !f.PayoffGatePassed && f.ProfitIfWinUSD > 0 {
		b.WriteString("• payoff gate: unenforced (MinProfitUSD=0 for tier)\n")
	}
	// Per-axis tail-gate explanation. The aggregate TailGatePassed flag
	// hides which axis was unenforceable; render per-axis so the
	// operator sees "market tail passed; trader tail unenforced" not the
	// misleading "no baseline was ready" when one of two axes was ready.
	writeTailGateStatus(b, f)
	if f.SeverityCapped {
		fmt.Fprintf(b, "• severity reduced to <b>%s</b> due to thin baseline\n", string(f.Severity))
	}
	if f.DormantWallet != nil && f.DormantWallet.IdleDuration > 0 {
		fmt.Fprintf(b, "• <b>dormant-wallet revival</b>: idle %s before this trade\n",
			humanDuration(f.DormantWallet.IdleDuration))
	}
	if f.Ownership != nil && f.Kind != anomaly.KindOwnership {
		// Fusion context — distinct from a standalone ownership_concentration
		// alert. The percentage is approximate (trade-flow proxy).
		fmt.Fprintf(b, "• <b>ownership context</b>: ~%.1f%% of recorded BUY flow (approx.)\n",
			f.Ownership.SharePct)
	}
	if f.LifecyclePct > 0 {
		hot := ""
		if f.Hot {
			hot = " (HOT — final stretch)"
		}
		fmt.Fprintf(b, "• market lifecycle: <b>%.1f%%</b> elapsed%s\n", f.LifecyclePct, hot)
	}
	writeQuietMarket(b, f)
	writeNewWallet(b, f)
	switch {
	case f.Kind == anomaly.KindTradeAnomaly && f.InCluster:
		fmt.Fprintf(b, "• <b>part of a forming cluster</b>: %d anomalous trades in the current window\n", f.ClusterPeerCount)
	case f.Kind == anomaly.KindTradeAnomaly:
		b.WriteString("• single trade (no peers in cluster window yet)\n")
	}
}

// writeQuietMarket renders the "quiet-market wake-up" context line when
// the corresponding gate qualified. Shared between single-trade and
// accumulation alerts so the operator-facing format is identical.
func writeQuietMarket(b *strings.Builder, f anomaly.Finding) {
	if f.QuietMarket == nil {
		return
	}
	q := f.QuietMarket
	idle := ""
	if q.IdleDuration > 0 {
		idle = ", idle " + humanDuration(q.IdleDuration)
	}
	fmt.Fprintf(b, "• <b>quiet-market wake-up</b>: historical activity ≈ %s trades/day, $%s/day%s\n",
		ratePerDay(q.TradesPerDay), money(q.NotionalPerDayUSD), idle)
}

// writeNewWallet renders the Strategy-B new-wallet context line on
// single-trade and accumulation alerts. Surveillance read: a $10k bet
// from a wallet first seen 4 hours ago with 2 prior trades is a
// qualitatively stronger informed-flow shape than the same trade from a
// long-history wallet.
func writeNewWallet(b *strings.Builder, f anomaly.Finding) {
	if f.NewWallet == nil || !f.NewWallet.IsNew {
		return
	}
	nw := f.NewWallet
	if !nw.FirstSeenAt.IsZero() && nw.AgeAtTrade > 0 {
		fmt.Fprintf(b, "• <b>new wallet</b>: first seen %s ago, %d stored trades\n",
			humanDuration(nw.AgeAtTrade), nw.HistoryTrades)
		return
	}
	// Fallback for the history-only path (FirstSeenAt missing).
	fmt.Fprintf(b, "• <b>new wallet</b>: %d stored trades\n", nw.HistoryTrades)
}

// writeTailRow renders a single tail row: "<label>: notional $N = <r95>x p95
// ($P95), <r99>x p99 ($P99)". Each ratio segment is elided when the input
// is zero so the formatter never claims "0x p99" — that would imply the
// gate exists with a 0 value, which is misleading.
func writeTailRow(b *strings.Builder, label string, notionalUSD, r95, p95 float64, r99, p99 float64) {
	fmt.Fprintf(b, "• %s: notional <b>$%s</b>", label, money(notionalUSD))
	if r95 > 0 && p95 > 0 {
		fmt.Fprintf(b, " = <b>%sx p95</b> ($%s)", ratioFmt(r95), money(p95))
	}
	if r99 > 0 && p99 > 0 {
		sep := ","
		if r95 <= 0 || p95 <= 0 {
			sep = " ="
		}
		fmt.Fprintf(b, "%s <b>%sx p99</b> ($%s)", sep, ratioFmt(r99), money(p99))
	}
	b.WriteByte('\n')
}

// percentileCell renders one percentile value as "$X" when positive
// and "n/a" when zero/unset. Zero is ambiguous (either small sample
// degenerate or pre-v6 payload missing the column); the truth is "we
// don't have this percentile", so render it that way instead of
// claiming "$0.00".
func percentileCell(v float64) string {
	if v <= 0 {
		return "n/a"
	}
	return "$" + money(v)
}

// writeTailGateStatus replaces the old "tail gate: unenforced (no
// baseline was ready)" line with per-axis explanations. The aggregate
// f.TailGatePassed flag in score.Result is true when ANY tail gate
// was enforced and passed — it hides cases where one axis was solid
// and the other was thin. We use the dedicated confidence flags
// instead.
func writeTailGateStatus(b *strings.Builder, f anomaly.Finding) {
	if f.LowMarketBaselineConfidence {
		if f.Baseline != nil && f.Baseline.SampleN > 0 {
			fmt.Fprintf(b, "• market tail: unenforced (baseline thin — %d samples)\n", f.Baseline.SampleN)
		} else {
			b.WriteString("• market tail: unenforced (no market baseline available)\n")
		}
	}
	if f.LowTraderBaselineConfidence {
		if f.TraderBaseline != nil && f.TraderBaseline.SampleN > 0 {
			fmt.Fprintf(b, "• trader tail: unenforced (only %d trader trades on record)\n", f.TraderBaseline.SampleN)
		} else {
			b.WriteString("• trader tail: unenforced (no trader history available)\n")
		}
	}
}

// ratioFmt prints with a decimal when below 10 so "0.65x" reads correctly,
// and as an integer above.
func ratioFmt(v float64) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// ratePerDay formats a per-day trade count. ≥10 → integer; otherwise one
// decimal (so 0.4 trades/day reads correctly).
func ratePerDay(v float64) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
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
// HTML <a href> anchor produced by renderLink — entries whose href is
// empty OR fails publicReachableURL validation are omitted entirely (no
// plain-text label is ever emitted). The whole section is skipped when
// nothing is renderable.
//
// publicReachableURL is the load-bearing defence: a typical operator misconfig
// leaves GRAFANA_BASE_URL pointing at http://localhost:3000, which is
// the docker-compose default. Telegram refuses to make localhost links
// clickable on mobile, so the alert recipient sees a useless dead-text
// "Grafana" entry. The validator fails closed (returns false) for
// localhost, loopback IPs, link-local IPs, and non-http(s) schemes.
func writeLinks(b *strings.Builder, f anomaly.Finding) {
	entries := []struct{ label, href string }{
		{"Polymarket market", sanitizeLinkURL(f.MarketURL)},
		{"Polymarket category", sanitizeLinkURL(f.CategoryURL)},
		{"Trader", sanitizeLinkURL(f.TraderURL)},
		{"Grafana", sanitizeLinkURL(f.GrafanaURL)},
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

// writeData renders the trailing <b>Data</b> block. The block carries the
// machine-readable identifiers an operator needs to correlate a Telegram
// alert with database rows, Grafana logs, or other systems:
//
//   - dedup: the polymarket_alerts.dedup_key for this finding. Embeds the
//     strategy version, so it is the primary key for any cross-system
//     join.
//   - market_id: condition_id (vo.MarketID) when known.
//   - outcome_token: the CLOB token id for the firing outcome.
//
// All values are HTML-escaped and rendered inside <code> so Telegram
// clients format them in a copy-friendly monospace font.
//
// writeAnalystNote replaces the v6 Data block. It renders the
// AI-generated analyst note when present. When the note is empty
// (AI layer disabled / rate-limited / errored), the block is
// elided entirely — there is no fallback to the old Data block.
//
// PART 4 of the Political-Catalyst Intelligence spec mandates that
// when a Blocked Alert block exists for the Finding, it MUST render
// ABOVE the analyst note. writeAnalystNote handles the ordering by
// delegating to writeBlockedAlertBlock first.
func writeAnalystNote(b *strings.Builder, f anomaly.Finding) {
	writeBlockedAlertBlock(b, f)
	note := strings.TrimSpace(f.AnalystNote)
	if note == "" {
		// Even with an empty AI body the annotations block can
		// stand on its own — render it before returning.
		writeRecentAnnotationsBlock(b, f)
		return
	}
	b.WriteString("\n<b>AI analysis</b>\n")
	// The note is plain text from the model; HTML-escape it so any
	// stray `<` or `>` doesn't break Telegram's HTML parser. Bullet
	// the first line so the formatting stays consistent with other
	// sections.
	for i, line := range strings.Split(note, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 {
			fmt.Fprintf(b, "• %s\n", html.EscapeString(line))
			continue
		}
		fmt.Fprintf(b, "  %s\n", html.EscapeString(line))
	}
	writeRecentAnnotationsBlock(b, f)
}

// writeRecentAnnotationsBlock renders up to 3 Polymarket event-page
// annotations BELOW the AI analysis. Polymarket-authored content is
// DATA — every field is HTML-escaped. Block elides entirely when
// f.RecentAnnotations is empty.
func writeRecentAnnotationsBlock(b *strings.Builder, f anomaly.Finding) {
	if len(f.RecentAnnotations) == 0 {
		return
	}
	const cap = 3
	b.WriteString("\n<b>Recent annotations</b>\n")
	n := len(f.RecentAnnotations)
	if n > cap {
		n = cap
	}
	for _, a := range f.RecentAnnotations[:n] {
		date := "—"
		if !a.Timestamp.IsZero() {
			date = a.Timestamp.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(b, "• %s · <b>%s</b>", html.EscapeString(date), html.EscapeString(strings.TrimSpace(a.Title)))
		if a.PriceBefore != nil && a.PriceAfter != nil {
			fmt.Fprintf(b, " · price %.2f→%.2f", *a.PriceBefore, *a.PriceAfter)
			if a.PriceChange != nil {
				fmt.Fprintf(b, " (%+.2f)", *a.PriceChange)
			}
		} else if a.PriceChange != nil {
			fmt.Fprintf(b, " · Δ%+.2f", *a.PriceChange)
		}
		if a.Outcome != "" {
			fmt.Fprintf(b, " · outcome=%s", html.EscapeString(a.Outcome))
		}
		b.WriteString("\n")
		if s := strings.TrimSpace(a.Summary); s != "" {
			if len(s) > 220 {
				s = s[:219] + "…"
			}
			fmt.Fprintf(b, "  %s\n", html.EscapeString(s))
		}
		if a.SourceName != "" {
			fmt.Fprintf(b, "  source: %s\n", html.EscapeString(a.SourceName))
		}
	}
}

// writeBlockedAlertBlock renders the Political-Catalyst Intelligence
// "Blocked Alert" payload. The block appears ABOVE the AI analysis
// when present so the operator sees the structural-uncertainty
// summary first.
//
// All fields are HTML-escaped: Polymarket-authored and AI-authored
// content is DATA, never markup. Empty fields elide. The whole
// block is skipped when f.Blocked is nil.
func writeBlockedAlertBlock(b *strings.Builder, f anomaly.Finding) {
	bl := f.Blocked
	if bl == nil {
		return
	}
	if bl.Status == "" && bl.Reason == "" && bl.CatalystType == "" && bl.BullishScenario == "" &&
		bl.BearishScenario == "" && bl.InvalidationScenario == "" && bl.Stance == "" {
		return
	}
	b.WriteString("\n<b>Blocked Alert</b>\n")
	if bl.Status != "" {
		fmt.Fprintf(b, "• status: %s\n", html.EscapeString(bl.Status))
	}
	if bl.Reason != "" {
		fmt.Fprintf(b, "• reason: %s\n", html.EscapeString(bl.Reason))
	}
	if bl.CatalystType != "" {
		fmt.Fprintf(b, "• catalyst type: %s\n", html.EscapeString(bl.CatalystType))
	}
	if bl.ExpectedTiming != "" {
		fmt.Fprintf(b, "• expected timing: %s\n", html.EscapeString(bl.ExpectedTiming))
	}
	if bl.BullishScenario != "" {
		fmt.Fprintf(b, "• bullish scenario: %s\n", html.EscapeString(bl.BullishScenario))
	}
	if bl.BearishScenario != "" {
		fmt.Fprintf(b, "• bearish scenario: %s\n", html.EscapeString(bl.BearishScenario))
	}
	if bl.InvalidationScenario != "" {
		fmt.Fprintf(b, "• invalidation scenario: %s\n", html.EscapeString(bl.InvalidationScenario))
	}
	if bl.Stance != "" {
		fmt.Fprintf(b, "• stance: %s\n", html.EscapeString(bl.Stance))
	}
}

// sanitizeLinkURL returns the URL when it is safe to put in front of an
// alert recipient and the empty string when it is not. Safe means:
//
//   - non-empty;
//   - parseable;
//   - scheme is http or https;
//   - host is not a loopback, link-local, or unspecified address;
//   - host is not the literal "localhost".
//
// Telegram silently refuses to make loopback links clickable on mobile,
// and an alert recipient seeing a dead "Grafana" bullet is worse than
// not seeing it at all. Returning empty here cascades through writeLinks
// → renderLink and the entry is elided.
//
// SanitizeLinkURL is the exported alias used by the prediction
// creation worker so we don't duplicate the unsafe-URL rules.
func SanitizeLinkURL(raw string) string { return sanitizeLinkURL(raw) }

func sanitizeLinkURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return ""
		}
	}
	return raw
}

// RenderLink is the exported alias used by the prediction creation
// worker — same sanitize + html.EscapeString contract as the
// per-alert renderer.
func RenderLink(label, href string) string { return renderLink(label, href) }

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
