// Package market models the Polymarket market universe and its categorization.
package market

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/vo"
)

// Market is the analytics-facing projection of a Gamma /markets row.
// Only the fields we actually use downstream are kept here; raw DTOs live in
// the adapter package.
type Market struct {
	ID          vo.MarketID
	Slug        string
	Question    string
	ConditionID string
	TokenIDs    []vo.TokenID
	Categories  []vo.CategoryID
	Active      bool
	Closed      bool
	StartDate   time.Time
	EndDate     time.Time
	Volume      float64 // lifetime, USD
	Volume24h   float64 // rolling 24h, USD (Gamma-reported)
	Liquidity   float64 // USD
}

// IsTradable returns true when the market is open and inside its trading window.
func (m Market) IsTradable(now time.Time) bool {
	if !m.Active || m.Closed {
		return false
	}
	if !m.StartDate.IsZero() && now.Before(m.StartDate) {
		return false
	}
	if !m.EndDate.IsZero() && now.After(m.EndDate) {
		return false
	}
	return true
}
