// Package vo holds value objects shared across domain aggregates.
package vo

// MarketID is the Polymarket condition id of a market (0x-prefixed hex).
type MarketID string

// TokenID is the CLOB token id (decimal string) for one outcome.
type TokenID string

// CategoryID is the Gamma tag id used to bucket markets.
type CategoryID int64

func (m MarketID) String() string { return string(m) }
func (t TokenID) String() string  { return string(t) }
