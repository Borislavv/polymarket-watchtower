package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Config tunes the client. Zero values fall through to sensible
// defaults so the worker doesn't have to fill every knob.
type Config struct {
	// Endpoint defaults to the public Polymarket CLOB market
	// channel. Overridable for fixture/staging.
	Endpoint string
	// MaxTokensHardCap is an EMERGENCY safety guard, NOT a normal
	// tuning knob. The selector caps by MARKETS (WS_MAX_MARKETS) and
	// every selected market is subscribed with ALL its outcome
	// tokens — no silent slicing. The hard cap exists purely so a
	// runaway selector (bug, mis-pinned config) cannot fan out a
	// 100k-token subscription before anyone notices.
	//
	// When the resolved token list exceeds MaxTokensHardCap, the
	// client returns an explicit ErrTokenHardCapExceeded so the
	// reconnect logic surfaces the failure loudly. Default 50000.
	//
	// Operators MUST NOT lower this to bound a "normal" subscription
	// — bound by WS_MAX_MARKETS instead. Tests pin the loud-failure
	// behaviour: see TestSubscribeOverHardCapFailsLoudly.
	MaxTokensHardCap int
	// PingInterval is the cadence at which we send the literal
	// "PING" text frame the Polymarket server expects.
	PingInterval time.Duration
	// ReadTimeout is the per-message read deadline. A missed PONG
	// trips this, the read returns err, and the client reconnects.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// ReconnectMin/Max backoff bracket. The client jittered-expo's
	// inside this range.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	// EventBuffer is the bounded send channel capacity. Drops
	// happen after this fills; the drop policy lives in the
	// realtime worker, not here.
	EventBuffer int
	// HTTPHeaders are forwarded on the WS upgrade request.
	HTTPHeaders http.Header
}

func (c *Config) applyDefaults() {
	if c.Endpoint == "" {
		c.Endpoint = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 10 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 45 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = 1 * time.Second
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = 30 * time.Second
	}
	if c.EventBuffer <= 0 {
		c.EventBuffer = 10_000
	}
	if c.MaxTokensHardCap <= 0 {
		c.MaxTokensHardCap = 50_000
	}
}

// ErrTokenHardCapExceeded is returned from connectOnce when the
// resolved token count exceeds MaxTokensHardCap. The reconnect loop
// must surface this error rather than swallow it — silent token
// truncation is the v11.12-insider-prior failure mode this guard
// exists to prevent.
var ErrTokenHardCapExceeded = errors.New("polymarket ws: subscription token count exceeds MaxTokensHardCap")

// Status enumerates the client's externally-observable health.
const (
	StatusDisabled     = "disabled"
	StatusConnecting   = "connecting"
	StatusConnected    = "connected"
	StatusReconnecting = "reconnecting"
	StatusStale        = "stale"
	StatusDisconnected = "disconnected"
)

// Client manages one WS connection. Reuse one per process — the
// realtime worker owns its lifecycle.
type Client struct {
	cfg    Config
	met    *metrics.Metrics
	log    *zerolog.Logger
	dialer *websocket.Dialer

	// status atomically holds the latest StatusXxx string for the
	// metrics layer + health endpoint.
	status     atomic.Value
	connected  atomic.Bool
	lastEvent  atomic.Int64 // unix nanos
	reconnects atomic.Int64

	// subMu guards `currentSubs` — read by the resolver, written
	// by Subscribe. Keep the write path short; the read path is
	// hot.
	subMu       sync.RWMutex
	currentSubs SubscriptionSet
	byTokenID   map[string]MarketSubscription
	byCondition map[string]MarketSubscription
}

// New constructs a Client. nil metrics + nil logger tolerated.
func New(cfg Config, met *metrics.Metrics, log *zerolog.Logger) *Client {
	cfg.applyDefaults()
	c := &Client{
		cfg: cfg,
		met: met,
		log: log,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			// Proxy + tls defaults are fine for the CLOB market
			// channel; no per-deployment override needed today.
		},
		byTokenID:   map[string]MarketSubscription{},
		byCondition: map[string]MarketSubscription{},
	}
	c.setStatus(StatusDisabled)
	return c
}

// Subscribe rebuilds the in-memory subscription index so future
// reconnects use the latest set. The active connection (if any) is
// not auto-resubscribed — Run sends a fresh subscribe payload on
// every connect; the worker drives a reconnect when it wants the
// set applied immediately.
func (c *Client) Subscribe(set SubscriptionSet) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.currentSubs = set
	c.byTokenID = map[string]MarketSubscription{}
	c.byCondition = map[string]MarketSubscription{}
	for _, m := range set.Markets {
		c.byCondition[m.ConditionID] = m
		for _, tok := range m.CLOBTokenIDs {
			c.byTokenID[strings.TrimSpace(tok)] = m
		}
	}
}

// Status returns the externally-observable connection state.
func (c *Client) Status() string {
	v, _ := c.status.Load().(string)
	if v == "" {
		return StatusDisabled
	}
	return v
}

// LastEventAge reports how long ago the last inbound message was.
// math.MaxInt64 when no event has been observed yet.
func (c *Client) LastEventAge(now time.Time) time.Duration {
	last := c.lastEvent.Load()
	if last == 0 {
		return 1<<62 - 1
	}
	return now.Sub(time.Unix(0, last))
}

// Run blocks until ctx cancels. Reconnects with jittered backoff on
// connection errors. Each connect:
//
//  1. Dials the endpoint.
//  2. Sends one subscribe payload (assets_ids = union of all subs).
//  3. Starts a writer goroutine that sends "PING" every PingInterval.
//  4. Reads, decodes, normalises, and forwards events to `out`.
//
// Drop policy: when `out` is full the event is dropped + the metric
// labelled "buffer_full" is incremented. The READ LOOP NEVER BLOCKS
// on the channel — Polymarket pumps faster than the worker can
// persist during bursts.
func (c *Client) Run(ctx context.Context, out chan<- Event) error {
	if len(c.currentSubs.Markets) == 0 {
		c.setStatus(StatusDisabled)
		return errors.New("ws.Run: no subscriptions configured")
	}
	backoff := c.cfg.ReconnectMin
	for {
		if ctx.Err() != nil {
			c.setStatus(StatusDisconnected)
			return ctx.Err()
		}
		c.setStatus(StatusConnecting)
		err := c.runOnce(ctx, out)
		if ctx.Err() != nil {
			c.setStatus(StatusDisconnected)
			return ctx.Err()
		}
		c.reconnects.Add(1)
		c.observeReconnect()
		if c.log != nil {
			c.log.Warn().Err(err).
				Dur("backoff", backoff).
				Msg("polymarket ws: connection ended, reconnecting")
		}
		c.setStatus(StatusReconnecting)
		select {
		case <-ctx.Done():
			c.setStatus(StatusDisconnected)
			return ctx.Err()
		case <-time.After(jittered(backoff)):
		}
		backoff *= 2
		if backoff > c.cfg.ReconnectMax {
			backoff = c.cfg.ReconnectMax
		}
	}
}

func (c *Client) runOnce(ctx context.Context, out chan<- Event) error {
	conn, _, err := c.dialer.DialContext(ctx, c.cfg.Endpoint, c.cfg.HTTPHeaders)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c.setStatus(StatusConnected)
	c.connected.Store(true)
	c.observeConnected(true)
	defer func() {
		c.connected.Store(false)
		c.observeConnected(false)
	}()

	c.subMu.RLock()
	currentSubs := c.currentSubs
	c.subMu.RUnlock()
	tokens, marketCount, err := resolveSubscribeTokens(currentSubs, c.cfg.MaxTokensHardCap)
	if err != nil {
		if c.log != nil {
			c.log.Error().
				Int("markets", marketCount).
				Int("hard_cap", c.cfg.MaxTokensHardCap).
				Msg("polymarket ws: subscription token count exceeds MaxTokensHardCap — refusing to subscribe; lower WS_MAX_MARKETS or raise WS_MAX_TOKENS_HARD_CAP if this is intentional")
		}
		return err
	}

	payload, _ := json.Marshal(subscribeMsg{AssetsIDs: tokens, Type: "market"})
	if err := c.writeText(conn, payload); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	c.observeSubscriptionCount(len(tokens))
	if c.log != nil {
		c.log.Info().
			Int("tokens", len(tokens)).
			Int("markets", marketCount).
			Str("endpoint", c.cfg.Endpoint).
			Msg("polymarket ws: subscribed")
	}

	// Writer goroutine: pings + cancellation watchdog.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		t := time.NewTicker(c.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.writeText(conn, []byte("PING")); err != nil {
					return
				}
			}
		}
	}()
	defer func() { <-writerDone }()
	defer conn.SetReadDeadline(time.Now()) //nolint:errcheck // unblock the reader on close

	// Read loop.
	for {
		if err := conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}
		now := time.Now()
		c.lastEvent.Store(now.UnixNano())
		ev := c.decodeOne(raw, now)
		c.observeEvent(ev.Type)
		if !c.send(out, ev) {
			c.observeDrop("buffer_full", ev.Type)
		}
	}
}

// decodeOne wraps decode() with the client's subscription resolver.
func (c *Client) decodeOne(raw []byte, now time.Time) Event {
	resolver := func(assetID, conditionID string) (MarketSubscription, bool) {
		c.subMu.RLock()
		defer c.subMu.RUnlock()
		if assetID != "" {
			if m, ok := c.byTokenID[strings.TrimSpace(assetID)]; ok {
				return m, true
			}
		}
		if conditionID != "" {
			if m, ok := c.byCondition[strings.TrimSpace(conditionID)]; ok {
				return m, true
			}
		}
		return MarketSubscription{}, false
	}
	return decode(raw, resolver, now)
}

// send is the non-blocking enqueue. Returns false when the
// destination is full so the caller can record a metric.
func (c *Client) send(out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	default:
		return false
	}
}

func (c *Client) writeText(conn *websocket.Conn, payload []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// jittered returns the supplied backoff multiplied by [0.5, 1.5).
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	jit := 0.5 + rand.Float64()
	return time.Duration(float64(d) * jit)
}

// --- metrics helpers (nil-safe) -------------------------------------------

func (c *Client) setStatus(s string) {
	c.status.Store(s)
	if c.met != nil && c.met.WSConnected != nil {
		v := 0.0
		if s == StatusConnected {
			v = 1
		}
		c.met.WSConnected.Set(v)
	}
}

func (c *Client) observeConnected(connected bool) {
	if c.met == nil || c.met.WSConnected == nil {
		return
	}
	if connected {
		c.met.WSConnected.Set(1)
	} else {
		c.met.WSConnected.Set(0)
	}
}

func (c *Client) observeReconnect() {
	if c.met == nil || c.met.WSReconnects == nil {
		return
	}
	c.met.WSReconnects.Inc()
}

func (c *Client) observeSubscriptionCount(n int) {
	if c.met == nil || c.met.WSSubscriptions == nil {
		return
	}
	c.met.WSSubscriptions.Set(float64(n))
}

func (c *Client) observeEvent(t string) {
	if c.met == nil || c.met.WSEvents == nil {
		return
	}
	c.met.WSEvents.WithLabelValues(t).Inc()
}

func (c *Client) observeDrop(reason, t string) {
	if c.met == nil || c.met.WSEventsDropped == nil {
		return
	}
	c.met.WSEventsDropped.WithLabelValues(reason, t).Inc()
}

// helper for tests / cli.
func MustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// resolveSubscribeTokens is the v11.12-insider-prior token resolver.
// Given the current SubscriptionSet, returns the deduped list of
// ALL token ids across ALL markets — no per-market truncation, no
// silent slicing. The only failure mode is the hard-cap circuit-
// breaker (default 50_000) which returns ErrTokenHardCapExceeded.
//
// Exposed for unit testing via subscribe payload tests; not part of
// the public API.
func resolveSubscribeTokens(set SubscriptionSet, hardCap int) ([]string, int, error) {
	marketCount := len(set.Markets)
	tokens := make([]string, 0)
	seen := make(map[string]struct{})
	for _, m := range set.Markets {
		for _, tok := range m.CLOBTokenIDs {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			tokens = append(tokens, tok)
		}
	}
	if hardCap > 0 && len(tokens) > hardCap {
		return nil, marketCount, ErrTokenHardCapExceeded
	}
	return tokens, marketCount, nil
}
