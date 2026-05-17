// Package alerting fans-out anomaly findings to one or more sinks.
package alerting

import (
	"context"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// Channel consumes findings. Implementations must be safe for concurrent use.
type Channel interface {
	Notify(ctx context.Context, f anomaly.Finding) error
	Name() string
}
