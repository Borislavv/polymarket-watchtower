// Package telegram is the Bot API HTTP client. It owns one job: marshal a
// pre-rendered HTML message to Telegram's /sendMessage endpoint and report
// the result.
//
// Things it explicitly does NOT own:
//   - alert/finding rendering — that lives next to the alerting domain;
//   - retry policy — the caller decides (the alert sender worker does it
//     by leaving the row pending and bumping send_attempts);
//   - subscriber discovery — Watchtower sends to a single configured chat.
//
// The package is a thin adapter, not an abstraction layer.
package telegram

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
	"time"
)

// Config wires a Bot. BotToken is required; BaseURL and Timeout have sane
// defaults so test/proxy overrides are the exception.
type Config struct {
	BotToken string
	BaseURL  string        // defaults to https://api.telegram.org
	Timeout  time.Duration // defaults to 5s
}

// Bot is the HTTP client. Safe for concurrent use.
type Bot struct {
	cfg    Config
	client *http.Client
}

// New validates the config and returns a ready Bot. An empty BotToken is a
// startup error — there is no useful "disabled" mode here; callers that
// might run without Telegram simply don't construct a Bot.
func New(cfg Config) (*Bot, error) {
	if strings.TrimSpace(cfg.BotToken) == "" {
		return nil, errors.New("telegram: bot token required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.telegram.org"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Bot{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

// SendResult is the post-send acknowledgement. MessageID is the Telegram
// server-assigned id for the delivered message; zero when the API replied
// without one (older proxies/mocks).
type SendResult struct {
	MessageID int64
}

// SendHTML posts one HTML-parse-mode message to chatID. chatID is sent as a
// JSON number when it parses as int64 (Telegram's preferred form for
// private/group chats) and as a string otherwise (channel @usernames).
func (b *Bot) SendHTML(ctx context.Context, chatID, text string) (SendResult, error) {
	if strings.TrimSpace(chatID) == "" {
		return SendResult{}, errors.New("telegram: chat id required")
	}
	body := map[string]any{
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if id, err := strconv.ParseInt(chatID, 10, 64); err == nil {
		body["chat_id"] = id
	} else {
		body["chat_id"] = chatID
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return SendResult{}, fmt.Errorf("telegram: marshal: %w", err)
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(b.cfg.BaseURL, "/"), b.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("telegram: send to chat %s: %w", chatID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return SendResult{}, fmt.Errorf("telegram: chat %s -> %d: %s", chatID, resp.StatusCode, string(snippet))
	}

	// Best-effort message_id extraction. Failure to decode a 2xx body is
	// not fatal — the message has already been accepted.
	var parsed struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&parsed); err != nil {
		return SendResult{}, nil
	}
	return SendResult{MessageID: parsed.Result.MessageID}, nil
}
