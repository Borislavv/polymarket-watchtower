// Package market models the Polymarket market universe and its categorization.
package market

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
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
	Outcomes    []string // human label per TokenIDs index ("Yes"/"No"/"Trump"/...)
	Categories  []vo.CategoryID
	Active      bool
	Closed      bool
	StartDate   time.Time
	EndDate     time.Time
	Volume      float64 // lifetime, USD
	Volume24h   float64 // rolling 24h, USD (Gamma-reported)
	Liquidity   float64 // USD
}

// OutcomeLabel resolves a token id to its human outcome label ("", "Yes", "No",
// etc.). Returns "" when the market doesn't carry outcome metadata or the
// token id is unknown.
func (m Market) OutcomeLabel(token vo.TokenID) string {
	for i, t := range m.TokenIDs {
		if t == token && i < len(m.Outcomes) {
			return m.Outcomes[i]
		}
	}
	return ""
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
