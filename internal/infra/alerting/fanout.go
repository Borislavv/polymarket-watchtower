package alerting

import (
	"context"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/rs/zerolog"
)

// Fanout broadcasts to multiple sinks; errors are logged but never block other
// sinks.
type Fanout struct {
	Sinks  []Channel
	Logger *zerolog.Logger
}

func (f *Fanout) Notify(ctx context.Context, finding anomaly.Finding) error {
	for _, s := range f.Sinks {
		if err := s.Notify(ctx, finding); err != nil {
			f.Logger.Err(err).Str("sink", s.Name()).Msg("alerting sink error")
		}
	}
	return nil
}
