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

// ErrEditUnsupported is returned by EditMessageText when Telegram
// explicitly rejects the edit (message too old per Bot API rules,
// "message is not modified", bot kicked, etc.). Callers persist
// this as terminal so a follow-up reply path can run instead.
var ErrEditUnsupported = errors.New("telegram: editMessageText unsupported on target")

// EditMessageText replaces the body of a previously-sent message
// identified by (chatID, messageID). Used by the outcome-learning
// worker to append a "Why WON / Why LOST" block to a resolved
// alert's original Telegram message.
//
// Telegram's `editMessageText` only succeeds when:
//   - the bot is the original sender
//   - the message is < 48h old (per Bot API current rules)
//   - the new text differs from the old text
//   - the bot still has rights in the chat
//
// Permission/capability rejections map to ErrEditUnsupported so the
// caller can fall back to a linked follow-up message. Generic
// transport errors are returned as ordinary errors.
//
// Telegram API reference:
//
//	POST https://api.telegram.org/bot<token>/editMessageText
//	  chat_id    int64 | "@channelusername"
//	  message_id int
//	  text       string
//	  parse_mode "HTML"
//	  disable_web_page_preview bool
func (b *Bot) EditMessageText(ctx context.Context, chatID string, messageID int64, text string) error {
	if strings.TrimSpace(chatID) == "" {
		return errors.New("telegram: chat id required")
	}
	if messageID <= 0 {
		return errors.New("telegram: message id required")
	}
	body := map[string]any{
		"message_id":               messageID,
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
		return fmt.Errorf("telegram: marshal: %w", err)
	}
	endpoint := fmt.Sprintf("%s/bot%s/editMessageText",
		strings.TrimRight(b.cfg.BaseURL, "/"), b.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: editMessageText: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	// 400 = parameter problem (message too old, not modified, parse
	// error, etc.) → terminal; map to ErrEditUnsupported so the
	// caller takes the follow-up-reply path. 403 = bot kicked /
	// channel rights revoked → also terminal. Everything else (429,
	// 5xx, network) is retryable.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %d: %s", ErrEditUnsupported, resp.StatusCode, string(snippet))
	}
	return fmt.Errorf("telegram: editMessageText chat %s -> %d: %s", chatID, resp.StatusCode, string(snippet))
}

// ErrReactionUnsupported is returned by SetMessageReaction when the
// Telegram API explicitly rejects the call because of capability /
// permission / target-type limits. Callers persist this as a terminal
// `unsupported` state and never retry. Generic transport / 5xx / rate-
// limit errors are returned as ordinary errors so the caller can retry.
var ErrReactionUnsupported = errors.New("telegram: setMessageReaction unsupported on target")

// SetMessageReaction posts a single emoji reaction onto the message
// identified by (chatID, messageID). Telegram's Bot API
// `setMessageReaction` accepts a `reaction` array of reaction-type
// objects; we pass exactly one ReactionTypeEmoji entry per call so the
// shape stays simple and only the chosen success/failure/ambiguous
// emoji ever appears.
//
// Permission / capability errors (channel reactions disabled, bot
// missing rights, paid reactions, emoji not in the allowed list)
// return ErrReactionUnsupported so callers mark the row terminally
// `unsupported`. Network / 5xx / rate-limit errors are returned as
// ordinary errors so the caller can retry.
//
// Telegram API reference:
//
//	POST https://api.telegram.org/bot<token>/setMessageReaction
//	  chat_id   int64 | "@channelusername"
//	  message_id int
//	  reaction  array of ReactionType (we send 1 emoji item)
//	  is_big    bool (false — we want quiet reactions on history)
//
// Per the Bot API: bot reactions require the bot to be a member of
// the chat. Channels with reactions disabled, private channels where
// the bot has no rights, and certain group setups will reject the
// call — that path is mapped to ErrReactionUnsupported.
func (b *Bot) SetMessageReaction(ctx context.Context, chatID string, messageID int64, emoji string) error {
	if strings.TrimSpace(chatID) == "" {
		return errors.New("telegram: chat id required")
	}
	if messageID <= 0 {
		return errors.New("telegram: message id required")
	}
	if strings.TrimSpace(emoji) == "" {
		return errors.New("telegram: emoji required")
	}
	body := map[string]any{
		"message_id": messageID,
		"reaction": []map[string]string{
			{"type": "emoji", "emoji": emoji},
		},
		"is_big": false,
	}
	if id, err := strconv.ParseInt(chatID, 10, 64); err == nil {
		body["chat_id"] = id
	} else {
		body["chat_id"] = chatID
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: marshal: %w", err)
	}
	endpoint := fmt.Sprintf("%s/bot%s/setMessageReaction",
		strings.TrimRight(b.cfg.BaseURL, "/"), b.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: setMessageReaction: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	bodyStr := string(snippet)
	// 400 / 403 with one of these substrings means the reaction cannot
	// be applied on this target — the caller persists `unsupported`
	// and stops trying.
	lower := strings.ToLower(bodyStr)
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
		for _, marker := range []string{
			"reaction_invalid",
			"reactions_too_many",
			"reactions_not_enabled",
			"chat_admin_required",
			"have no rights",
			"not enough rights",
			"message_not_modified", // already reacted, treat as success-ish — but cleaner as unsupported so we stop
			"message to react not found",
			"bot was kicked",
			"bot is not a member",
		} {
			if strings.Contains(lower, marker) {
				return fmt.Errorf("%w: %s", ErrReactionUnsupported, bodyStr)
			}
		}
	}
	return fmt.Errorf("telegram: setMessageReaction chat=%s msg=%d -> %d: %s",
		chatID, messageID, resp.StatusCode, bodyStr)
}
