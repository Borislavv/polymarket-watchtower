package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// PollerConfig wires the Telegram /getUpdates loop.
type PollerConfig struct {
	BotToken string
	BaseURL  string        // defaults to https://api.telegram.org
	Interval time.Duration // tick period; defaults to 10s
	Timeout  time.Duration // per-request HTTP timeout; defaults to 5s
}

// Poller periodically calls Telegram /getUpdates, extracts chat ids from each
// update kind (message / edited_message / channel_post / edited_channel_post /
// my_chat_member), and adds them to a shared Subscribers registry. It
// acknowledges each batch by advancing the offset so updates aren't replayed.
//
// Transient HTTP / decode failures are logged and retried on the next tick —
// the loop is best-effort and never blocks the alerting path.
type Poller struct {
	cfg    PollerConfig
	subs   *Subscribers
	client *http.Client
	log    *zerolog.Logger

	// offset is the next update_id we want to receive; persists across ticks
	// within a single process lifetime. Acknowledging via offset clears the
	// server-side queue so it doesn't grow unbounded.
	offset int64
}

// NewPoller validates required fields. subs must be non-nil.
func NewPoller(cfg PollerConfig, subs *Subscribers, log *zerolog.Logger) (*Poller, error) {
	if cfg.BotToken == "" {
		return nil, errors.New("telegram poller: bot token required")
	}
	if subs == nil {
		return nil, errors.New("telegram poller: subscribers registry required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.telegram.org"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Poller{
		cfg:    cfg,
		subs:   subs,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log,
	}, nil
}

// Run drives the poller until ctx is cancelled. Errors are logged, never
// returned — this loop is supervised by the app's graceful-shutdown harness.
func (p *Poller) Run(ctx context.Context) error {
	// One immediate tick so the bot's existing backlog is consumed at startup,
	// then settle into the configured cadence.
	if err := p.tick(ctx); err != nil && p.log != nil {
		p.log.Warn().Err(err).Msg("telegram poller: initial tick failed")
	}
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := p.tick(ctx); err != nil && p.log != nil {
				p.log.Warn().Err(err).Msg("telegram poller: tick failed")
			}
		}
	}
}

// Tick performs one /getUpdates call; exposed for tests so they can drive the
// poller deterministically without the ticker.
func (p *Poller) Tick(ctx context.Context) error { return p.tick(ctx) }

func (p *Poller) tick(ctx context.Context) error {
	q := url.Values{}
	q.Set("timeout", "0") // short-poll; the app already has its own cadence
	if p.offset > 0 {
		q.Set("offset", strconv.FormatInt(p.offset, 10))
	}
	q.Set("allowed_updates", `["message","edited_message","channel_post","edited_channel_post","my_chat_member"]`)

	endpoint := fmt.Sprintf("%s/bot%s/getUpdates?%s",
		strings.TrimRight(p.cfg.BaseURL, "/"), p.cfg.BotToken, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("getUpdates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("getUpdates: read body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("getUpdates: status %d: %s", resp.StatusCode, string(body))
	}

	var env updatesEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("getUpdates: decode: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("getUpdates: not ok: %s", env.Description)
	}

	for _, u := range env.Result {
		if u.UpdateID >= p.offset {
			p.offset = u.UpdateID + 1
		}
		if id, ok := chatIDFromUpdate(u); ok {
			if p.subs.Add(id) && p.log != nil {
				p.log.Info().Int64("chat_id", id).Msg("telegram poller: new subscriber")
			}
		}
	}
	return nil
}

// --- Telegram DTOs (subset) -------------------------------------------------

type updatesEnvelope struct {
	OK          bool             `json:"ok"`
	Description string           `json:"description,omitempty"`
	Result      []telegramUpdate `json:"result"`
}

type telegramUpdate struct {
	UpdateID          int64    `json:"update_id"`
	Message           *chatRef `json:"message,omitempty"`
	EditedMessage     *chatRef `json:"edited_message,omitempty"`
	ChannelPost       *chatRef `json:"channel_post,omitempty"`
	EditedChannelPost *chatRef `json:"edited_channel_post,omitempty"`
	MyChatMember      *chatRef `json:"my_chat_member,omitempty"`
}

type chatRef struct {
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

func chatIDFromUpdate(u telegramUpdate) (int64, bool) {
	for _, ref := range []*chatRef{u.Message, u.EditedMessage, u.ChannelPost, u.EditedChannelPost, u.MyChatMember} {
		if ref != nil && ref.Chat.ID != 0 {
			return ref.Chat.ID, true
		}
	}
	return 0, false
}
