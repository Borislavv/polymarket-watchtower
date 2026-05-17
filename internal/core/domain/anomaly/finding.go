// Package anomaly models spike-detection rules and their results.
package anomaly

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/vo"
)

// Scope identifies what an anomaly is about (a market or a category).
type Scope string

const (
	ScopeMarket   Scope = "market"
	ScopeCategory Scope = "category"
)

// Metric is what the detector evaluates a multiplier against.
type Metric string

const (
	MetricTradeRate    Metric = "trade_rate"    // trades per minute
	MetricNotionalRate Metric = "notional_rate" // USD per minute
	MetricAvgSize      Metric = "avg_size"      // average size per trade
)

// Severity is a coarse classification used by metrics labels and downstream
// routing. Severities map to the configured multiplier ladder.
type Severity string

const (
	SeverityWarn     Severity = "warn"     // >= lowest multiplier
	SeverityCritical Severity = "critical" // >= middle multiplier
	SeverityFatal    Severity = "fatal"    // >= top multiplier
)

// Finding is a single anomaly event.
type Finding struct {
	At          time.Time
	Scope       Scope
	Market      vo.MarketID
	Category    vo.CategoryID
	Label       string
	MarketURL   string // best-effort polymarket.com link; empty for category scope
	Metric      Metric
	Severity    Severity
	Multiplier  float64 // observed recent/baseline ratio
	Recent      float64 // recent metric value
	Baseline    float64 // baseline metric value
	WindowLen   time.Duration
	BaselineLen time.Duration
}
