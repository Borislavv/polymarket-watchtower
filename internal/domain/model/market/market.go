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
	// EventSlug is the Gamma event the market belongs to. The user-facing
	// Polymarket URL is /event/<EventSlug> — the market slug alone is NOT a
	// valid page (returns 404 for any market grouped under a multi-outcome
	// event, e.g. "Will Tunisia win the FIFA World Cup?" lives under the
	// "2026 FIFA World Cup Winner" event).
	EventSlug  string
	EventTitle string
	Active     bool
	Closed     bool
	StartDate  time.Time
	EndDate    time.Time
	Volume     float64 // lifetime, USD
	Volume24h  float64 // rolling 24h, USD (Gamma-reported)
	Liquidity  float64 // USD
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

// LifecyclePct returns where now sits inside [StartDate, EndDate] as a 0–100
// percentage, plus ok=true when both dates are present and total span > 0.
// Markets without lifecycle metadata cannot be gated; callers should pass them
// through by default.
func (m Market) LifecyclePct(now time.Time) (float64, bool) {
	if m.StartDate.IsZero() || m.EndDate.IsZero() {
		return 0, false
	}
	total := m.EndDate.Sub(m.StartDate)
	if total <= 0 {
		return 0, false
	}
	pct := float64(now.Sub(m.StartDate)) / float64(total) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
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
