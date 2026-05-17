package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TelegramConfig is the minimal config for a Telegram bot.
type TelegramConfig struct {
	Enabled  bool
	BotToken string
	ChatID   string
	BaseURL  string
	Timeout  time.Duration
}

// Subscribers is a concurrency-safe set of Telegram chat ids.
type Subscribers struct {
	mu  sync.RWMutex
	ids map[int64]struct{}
}

func NewSubscribers(seed ...string) *Subscribers {
	s := &Subscribers{ids: make(map[int64]struct{})}
	for _, raw := range seed {
		if raw == "" {
			continue
		}
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			s.ids[id] = struct{}{}
		}
	}
	return s
}

func (s *Subscribers) Add(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return false
	}
	s.ids[id] = struct{}{}
	return true
}

func (s *Subscribers) Snapshot() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]int64, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

func (s *Subscribers) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ids)
}

// TelegramSink broadcasts findings to every chat in the Subscribers registry.
type TelegramSink struct {
	cfg         TelegramConfig
	client      *http.Client
	subscribers *Subscribers
	metrics     *metrics.Metrics
}

func NewTelegramSink(cfg TelegramConfig, subs *Subscribers) (*TelegramSink, error) {
	if !cfg.Enabled {
		return &TelegramSink{cfg: cfg, subscribers: subs}, nil
	}
	if cfg.BotToken == "" {
		return nil, errors.New("telegram: bot token required when enabled")
	}
	if subs == nil {
		return nil, errors.New("telegram: subscribers registry required when enabled")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.telegram.org"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &TelegramSink{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}, subscribers: subs}, nil
}

func (s *TelegramSink) WithMetrics(m *metrics.Metrics) *TelegramSink {
	s.metrics = m
	return s
}

func (s *TelegramSink) Name() string { return "telegram" }

func (s *TelegramSink) Notify(ctx context.Context, f anomaly.Finding) error {
	if !s.cfg.Enabled {
		return nil
	}
	chatIDs := s.subscribers.Snapshot()
	if len(chatIDs) == 0 {
		return nil
	}
	text := FormatTelegramMessage(f)
	var firstErr error
	for _, id := range chatIDs {
		if err := s.sendOne(ctx, id, text); err != nil {
			s.observeErr(f.Severity)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.observeOK(f.Severity)
	}
	return firstErr
}

func (s *TelegramSink) sendOne(ctx context.Context, chatID int64, text string) error {
	body := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(s.cfg.BaseURL, "/"), s.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send to chat %d: %w", chatID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram: chat %d -> %d: %s", chatID, resp.StatusCode, string(b))
	}
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
	default:
		return formatTradeAnomaly(f)
	}
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
		multiplierFmt(f.Multiplier),
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
	if f.Multiplier > 0 && f.Baseline != nil {
		fmt.Fprintf(b, "• <b>x%s</b> above baseline median ($%s)\n",
			multiplierFmt(f.Multiplier), money(f.Baseline.MedianUSD))
	}
	if f.Trade != nil && f.Trade.Odds > 0 {
		fmt.Fprintf(b, "• odds <b>%s</b>, implied probability <b>%.1f%%</b>\n",
			multiplierFmt(f.Trade.Odds), f.Trade.Price*100)
	}
	if f.Baseline != nil {
		fmt.Fprintf(b, "• baseline: <b>%d</b> trades, median $%s, mean $%s, p95 $%s, window %s\n",
			f.Baseline.SampleN, money(f.Baseline.MedianUSD), money(f.Baseline.MeanUSD), money(f.Baseline.P95USD), f.Baseline.WindowAgo)
	}
	if f.AbsoluteTier != "" || f.MultiplierTier != "" {
		fmt.Fprintf(b, "• tiers: absolute=<code>%s</code> multiplier=<code>%s</code> → final=<b>%s</b>\n",
			string(f.AbsoluteTier), string(f.MultiplierTier), string(f.Severity))
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

func writeLinks(b *strings.Builder, f anomaly.Finding) {
	if f.MarketURL == "" && f.CategoryURL == "" && f.TraderURL == "" && f.GrafanaURL == "" {
		return
	}
	b.WriteString("\n<b>Links</b>\n")
	type linkEntry struct {
		label, href string
	}
	for _, e := range []linkEntry{
		{"Polymarket market", f.MarketURL},
		{"Polymarket category", f.CategoryURL},
		{"Trader", f.TraderURL},
		{"Grafana", f.GrafanaURL},
	} {
		if e.href == "" {
			continue
		}
		fmt.Fprintf(b, `• <a href="%s">%s</a>`+"\n", html.EscapeString(e.href), e.label)
	}
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
