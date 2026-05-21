// Package newsintel runs the v11.0 Hourly News Intelligence cycle.
//
// One AI call per hour over NEW Polymarket annotations with linked
// affected markets. Replaces the prediction-evolution + market-intel
// surfaces killed by the v11.0 product cut. No predictions, no
// market-overview filler, no retrospective commentary.
//
// Failure semantics: every layer fails open. Zero new news → no AI
// call, no Telegram, row stamped status=skipped. AI sentinel → no
// Telegram, row stamped status=ok with the sentinel code. AI failure
// → row stamped status=failed, no Telegram. Telegram failure → row
// stamped telegram_sent=false with last_error.
package newsintel

import "time"

// Config is the operator-facing knob set. Defaults applied by
// applyDefaults so a partially-populated struct is still usable.
type Config struct {
	Enabled            bool
	StartupRun         bool
	Interval           time.Duration
	Lookback           time.Duration
	MaxItems           int
	MaxMarketsPerItem  int
	MaxSelected        int
	AIEnabled          bool
	AITimeout          time.Duration
	SendTelegram       bool
	SuppressNoEdge     bool
	DedupeEnabled      bool
	SemanticCooldown   time.Duration
	MinConfidence      float64
	ChatID             string
	TelegramMessageCap int

	// Clock is overridable for tests.
	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.Lookback <= 0 {
		c.Lookback = time.Hour
	}
	if c.MaxItems <= 0 {
		c.MaxItems = 100
	}
	if c.MaxMarketsPerItem <= 0 {
		c.MaxMarketsPerItem = 5
	}
	if c.MaxSelected <= 0 {
		c.MaxSelected = 8
	}
	if c.AITimeout <= 0 {
		c.AITimeout = 60 * time.Second
	}
	if c.SemanticCooldown <= 0 {
		c.SemanticCooldown = 12 * time.Hour
	}
	if c.MinConfidence < 0 {
		c.MinConfidence = 0
	}
	if c.MinConfidence > 1 {
		c.MinConfidence = 1
	}
	if c.TelegramMessageCap <= 0 {
		c.TelegramMessageCap = 3500
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}
