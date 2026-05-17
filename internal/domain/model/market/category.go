package market

import (
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
)

// Category is a Polymarket Gamma tag, used as the analytic "category" dimension.
type Category struct {
	ID    vo.CategoryID
	Slug  string
	Label string
}
