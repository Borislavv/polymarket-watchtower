package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/anomaly"
)

// TelegramConfig is the minimal config for a single-channel Telegram bot.
// BaseURL defaults to the public Telegram API but is configurable so tests can
// point it at an httptest.Server.
type TelegramConfig struct {
	Enabled  bool
	BotToken string
	ChatID   string
	BaseURL  string
	Timeout  time.Duration
}

// TelegramSink posts findings to a single Telegram chat. It is a no-op when
// Config.Enabled is false; this lets the app wire it unconditionally.
type TelegramSink struct {
	cfg    TelegramConfig
	client *http.Client
}

// NewTelegramSink validates config and returns a sink. When Enabled is false
// it still returns a valid sink whose Notify is a no-op.
func NewTelegramSink(cfg TelegramConfig) (*TelegramSink, error) {
	if !cfg.Enabled {
		return &TelegramSink{cfg: cfg}, nil
	}
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return nil, errors.New("telegram: bot token and chat id required when enabled")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.telegram.org"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &TelegramSink{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (s *TelegramSink) Name() string { return "telegram" }

// Notify formats and sends one Telegram message.
func (s *TelegramSink) Notify(ctx context.Context, f anomaly.Finding) error {
	if !s.cfg.Enabled {
		return nil
	}

	body := map[string]any{
		"chat_id":                  s.cfg.ChatID,
		"text":                     FormatTelegramMessage(f),
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
		return fmt.Errorf("telegram: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram: %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// FormatTelegramMessage produces the Markdown-formatted body for one finding.
// Exposed so tests can assert on it without invoking the HTTP path.
func FormatTelegramMessage(f anomaly.Finding) string {
	var b strings.Builder
	icon := severityIcon(f.Severity)
	fmt.Fprintf(&b, "%s *Polymarket %s anomaly*\n", icon, strings.ToUpper(string(f.Severity)))
	fmt.Fprintf(&b, "_%s_ on `%s`\n", f.Metric, scopeTarget(f))
	if f.Label != "" {
		fmt.Fprintf(&b, "*%s*\n", escapeMarkdown(f.Label))
	}
	fmt.Fprintf(&b, "multiplier: *x%.1f*\n", f.Multiplier)
	fmt.Fprintf(&b, "recent: `%.4g`  baseline: `%.4g`\n", f.Recent, f.Baseline)
	fmt.Fprintf(&b, "window: %s  baseline window: %s\n", f.WindowLen, f.BaselineLen)
	fmt.Fprintf(&b, "at: %s\n", f.At.UTC().Format(time.RFC3339))
	if f.MarketURL != "" {
		fmt.Fprintf(&b, "[open market](%s)", f.MarketURL)
	}
	return b.String()
}

func scopeTarget(f anomaly.Finding) string {
	if f.Scope == anomaly.ScopeMarket && f.Market != "" {
		return string(f.Market)
	}
	return fmt.Sprintf("category:%d", f.Category)
}

func severityIcon(s anomaly.Severity) string {
	switch s {
	case anomaly.SeverityFatal:
		return "[CRIT]"
	case anomaly.SeverityCritical:
		return "[WARN]"
	default:
		return "[INFO]"
	}
}

// escapeMarkdown blunts the legacy Markdown parser's special characters so
// market questions don't break the rendering. The legacy "Markdown" parse
// mode has fewer escape rules than MarkdownV2.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer("_", `\_`, "*", `\*`, "`", "\\`", "[", `\[`)
	return r.Replace(s)
}
