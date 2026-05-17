package shutdown

import (
	"time"

	"github.com/rs/zerolog"
)

type config struct {
	logger          *zerolog.Logger
	fadeOutDuration time.Duration
}
