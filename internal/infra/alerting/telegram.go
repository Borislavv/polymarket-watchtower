package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// TelegramConfig is the minimal config for a Telegram bot. ChatIDs are sourced
// from two places, deduplicated at send time:
//   - The optional static ChatID seeded at construction.
//   - Dynamic subscribers discovered via Poller (/getUpdates) added at runtime.
type TelegramConfig struct {
	Enabled  bool
	BotToken string
	ChatID   string // static seed; empty is OK when a Poller is running
	BaseURL  string
	Timeout  time.Duration
}

// Subscribers is a concurrency-safe set of Telegram chat ids. The sink reads
// it on every send, the poller writes to it as new chats interact with the bot.
type Subscribers struct {
	mu  sync.RWMutex
	ids map[int64]struct{}
}

// NewSubscribers builds a registry pre-seeded with the supplied chat ids.
// Empty / unparseable inputs are skipped silently.
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

// Add records a chat id; returns true when it wasn't already known.
func (s *Subscribers) Add(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return false
	}
	s.ids[id] = struct{}{}
	return true
}

// Snapshot returns a stable copy of the chat ids. Safe to iterate while the
// registry is being mutated.
func (s *Subscribers) Snapshot() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]int64, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

// Size is the current subscriber count.
func (s *Subscribers) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ids)
}

// TelegramSink broadcasts findings to every chat in the Subscribers registry.
// A disabled sink (Config.Enabled == false) is a no-op so the app can wire it
// unconditionally.
type TelegramSink struct {
	cfg         TelegramConfig
	client      *http.Client
	subscribers *Subscribers
	metrics     *metrics.Metrics // optional; nil-safe
}

// NewTelegramSink validates config and returns a sink. The supplied Subscribers
// must be non-nil when Enabled is true.
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
	return &TelegramSink{
		cfg:         cfg,
		client:      &http.Client{Timeout: cfg.Timeout},
		subscribers: subs,
	}, nil
}

// WithMetrics attaches a metrics handle for send success / failure counters.
func (s *TelegramSink) WithMetrics(m *metrics.Metrics) *TelegramSink {
	s.metrics = m
	return s
}

func (s *TelegramSink) Name() string { return "telegram" }

// Notify broadcasts the finding to every known chat. Per-chat failures are
// logged via metrics and don't abort the broadcast. Returns the first error
// (so the Fanout records something), but all chats are attempted.
func (s *TelegramSink) Notify(ctx context.Context, f anomaly.Finding) error {
	if !s.cfg.Enabled {
		return nil
	}
	chatIDs := s.subscribers.Snapshot()
	if len(chatIDs) == 0 {
		// Nothing to send to yet — silent rather than spamming an error per
		// alert before any user has interacted with the bot.
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
		"parse_mode":               "Markdown",
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

// FormatTelegramMessage renders the Markdown body for one finding. The format
// is tuned for a human reviewing the alert on a phone — what / where / why /
// dynamics / links, in that order. Exposed so tests can assert on it without
// invoking HTTP.
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
	fmt.Fprintf(&b, "%s *Polymarket %s — single bet anomaly*\n", severityBadge(f.Severity), strings.ToUpper(string(f.Severity)))
	if f.Reason != "" {
		fmt.Fprintf(&b, "_rule: %s_\n", escapeMarkdown(f.Reason))
	}
	if f.Trade != nil {
		t := f.Trade
		title := t.Question
		if title == "" {
			title = t.Slug
		}
		if title == "" {
			title = string(t.Market)
		}
		fmt.Fprintf(&b, "*%s*\n", escapeMarkdown(title))
		if t.Outcome != "" || t.Side != "" {
			fmt.Fprintf(&b, "outcome: `%s`  side: `%s`\n", escapeMarkdown(t.Outcome), escapeMarkdown(string(t.Side)))
		}
		fmt.Fprintf(&b, "size: *$%s*  (`%.2f` @ `%.4f`)\n", money(t.NotionalUSD), t.SizeShares, t.Price)
		if t.Wallet != "" {
			fmt.Fprintf(&b, "wallet: `%s`\n", t.Wallet)
		}
	}
	if f.Category != nil && f.Category.Label != "" {
		fmt.Fprintf(&b, "category: *%s*\n", escapeMarkdown(f.Category.Label))
	}
	if f.Baseline != nil {
		bs := f.Baseline
		fmt.Fprintf(&b, "baseline: median *$%s*  mean *$%s*  p95 *$%s*  N=`%d`  window=`%s`\n",
			money(bs.MedianUSD), money(bs.MeanUSD), money(bs.P95USD), bs.SampleN, bs.WindowAgo)
	}
	if f.Multiplier > 0 {
		fmt.Fprintf(&b, "multiplier: *x%s*\n", multiplierFmt(f.Multiplier))
	}
	if f.AbsoluteTier > 0 {
		fmt.Fprintf(&b, "absolute tier crossed: *$%s*\n", money(f.AbsoluteTier))
	}
	fmt.Fprintf(&b, "at: `%s`\n", f.At.UTC().Format(time.RFC3339))
	appendLinks(&b, f)
	return b.String()
}

func formatCategoryWatch(f anomaly.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s *Polymarket — CATEGORY WATCH REQUIRED*\n", severityBadge(f.Severity))
	if f.Category != nil {
		fmt.Fprintf(&b, "category: *%s*\n", escapeMarkdown(nonEmpty(f.Category.Label, fmt.Sprintf("id=%d", f.Category.ID))))
	}
	if f.Cluster != nil {
		c := f.Cluster
		fmt.Fprintf(&b, "*%d anomalous trades* from *%d unique wallets* totalling *$%s* in the last `%s`\n",
			c.AnomalousTrades, c.UniqueWallets, money(c.TotalUSD), c.Window)
		if len(c.Sample) > 0 {
			b.WriteString("\nrecent contributors:\n")
			for _, t := range c.Sample {
				title := t.Question
				if title == "" {
					title = t.Slug
				}
				if title == "" {
					title = string(t.Market)
				}
				fmt.Fprintf(&b, "  • *$%s* on `%s` — %s `%s`\n",
					money(t.NotionalUSD), escapeMarkdown(title), shortWallet(t.Wallet), escapeMarkdown(t.Outcome))
			}
		}
	}
	fmt.Fprintf(&b, "\nat: `%s`\n", f.At.UTC().Format(time.RFC3339))
	appendLinks(&b, f)
	return b.String()
}

func appendLinks(b *strings.Builder, f anomaly.Finding) {
	if f.MarketURL != "" {
		fmt.Fprintf(b, "[open market](%s)", f.MarketURL)
	}
	if f.GrafanaURL != "" {
		if f.MarketURL != "" {
			b.WriteString("  •  ")
		}
		fmt.Fprintf(b, "[open in Grafana](%s)", f.GrafanaURL)
	}
}

// severityBadge returns a short uppercased label for the alert header. We
// deliberately avoid `[` because Telegram's legacy "Markdown" parse mode reads
// it as the start of a `[text](url)` link and rejects the whole message with
// a 400 ("can't parse entities").
func severityBadge(s anomaly.Severity) string {
	switch s {
	case anomaly.SeverityHard:
		return "HARD"
	case anomaly.SeverityCritical:
		return "CRIT"
	case anomaly.SeverityWarning:
		return "WARN"
	case anomaly.SeverityInfo:
		return "INFO"
	}
	return "ANOM"
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	case m >= 1000:
		return fmt.Sprintf("%.0f", m)
	case m >= 100:
		return fmt.Sprintf("%.0f", m)
	case m >= 10:
		return fmt.Sprintf("%.1f", m)
	default:
		return fmt.Sprintf("%.2f", m)
	}
}

// escapeMarkdown blunts the legacy "Markdown" parse-mode reserved chars so
// market questions don't break rendering.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer("_", `\_`, "*", `\*`, "`", "\\`", "[", `\[`)
	return r.Replace(s)
}
