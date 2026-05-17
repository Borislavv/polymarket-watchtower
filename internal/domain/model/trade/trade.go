// Package trade models executed trades pulled from the Polymarket Data API.
package trade

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Side is the trade direction from the taker's perspective.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Trade is the analytics-facing projection of a Data API trade.
type Trade struct {
	ID        string
	Market    vo.MarketID
	Token     vo.TokenID
	Side      Side
	Price     float64 // [0,1] probability
	Size      float64 // shares
	Timestamp time.Time
	TxHash    string
	Taker     string
	Maker     string
}

// NotionalUSD returns size * price, which on Polymarket is the USDC notional
// transacted for this trade (probabilities are 0..1 and each share pays $1).
func (t Trade) NotionalUSD() float64 { return t.Size * t.Price }
