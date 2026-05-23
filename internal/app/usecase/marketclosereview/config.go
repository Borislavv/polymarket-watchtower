// Package marketclosereview runs the v11.4 Market Close Review
// learning loop. When a Polymarket market closes/resolves the
// worker collects compact evidence (alerts, flow aggregates,
// events) and asks a strict-JSON AI prompt to judge whether
// Watchtower's alerts caught real informed flow.
//
// Hard rules:
//   - Admin destination only. The router routes the
//     SurfaceMarketCloseReview body to TELEGRAM_ADMIN_CHAT_ID;
//     it NEVER falls back to the signal chat.
//   - AI is budget-gated. The worker calls aibudget.Allow on the
//     "market_close_review" bucket before every dispatch.
//   - Reviews are append-only. Failed attempts retry with
//     backoff; once succeeded the row is terminal.
//   - Reactions are applied only to alerts that have a stored
//     TelegramMessageID and only on the signal chat. Reaction
//     failures NEVER fail the review.
//
// Fail-open everywhere — the alert pipeline is fully decoupled
// from this worker.
package marketclosereview

import "time"

// Config tunes the worker. Mirrors the operator env vars under
// MARKET_CLOSE_REVIEW_* (see internal/app/config.go).
type Config struct {
	Enabled                bool
	Interval               time.Duration
	Lookback               time.Duration
	MarketMaxAgeAfterClose time.Duration
	HistoryLookback        time.Duration
	MinAlerts              int
	RequireAlertOrNews     bool
	MaxMarketsPerRun       int
	MaxAlertsPerMarket     int
	MaxEventsPerMarket     int
	AIEnabled              bool
	AITimeout              time.Duration
	AIModel                string
	DailyBudgetUSD         float64
	SendAdminTelegram      bool
	SetReactions           bool
	ReactionSuccess        string
	ReactionFailure        string
	ReactionAmbiguous      string
	ReactionSkipAmbiguous  bool
	SignalChatID           string // for reactions (matches alert.TelegramMessageID chat)

	// Retry policy for failed reviews. Operator-tunable later;
	// the worker uses these as in-process defaults.
	RetryInitialBackoff time.Duration
	RetryMaxBackoff     time.Duration

	Clock func() time.Time
}

// applyDefaults fills zero-value fields. Mirrors the values the
// spec documents under MARKET_CLOSE_REVIEW_*.
func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Minute
	}
	if c.Lookback <= 0 {
		c.Lookback = 24 * time.Hour
	}
	if c.MarketMaxAgeAfterClose <= 0 {
		c.MarketMaxAgeAfterClose = 72 * time.Hour
	}
	if c.HistoryLookback <= 0 {
		c.HistoryLookback = 8760 * time.Hour
	}
	if c.MinAlerts <= 0 {
		c.MinAlerts = 1
	}
	if c.MaxMarketsPerRun <= 0 {
		c.MaxMarketsPerRun = 10
	}
	if c.MaxAlertsPerMarket <= 0 {
		c.MaxAlertsPerMarket = 50
	}
	if c.MaxEventsPerMarket <= 0 {
		c.MaxEventsPerMarket = 30
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 60 * time.Second
	}
	if c.DailyBudgetUSD <= 0 {
		c.DailyBudgetUSD = 3
	}
	if c.ReactionSuccess == "" {
		c.ReactionSuccess = "👍"
	}
	if c.ReactionFailure == "" {
		c.ReactionFailure = "👎"
	}
	if c.ReactionAmbiguous == "" {
		c.ReactionAmbiguous = "🤔"
	}
	if c.RetryInitialBackoff <= 0 {
		c.RetryInitialBackoff = 1 * time.Hour
	}
	if c.RetryMaxBackoff <= 0 {
		c.RetryMaxBackoff = 12 * time.Hour
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}
