package shutdown

import (
	"time"

	"github.com/rs/zerolog"
)

type Options = func(config) config

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

var defaultOptions = []Options{
	WithLogger(nopLogger()),
	WithFadeOutDuration(5 * time.Second),
}

func WithLogger(logger *zerolog.Logger) Options {
	return func(c config) config {
		c.logger = logger
		return c
	}
}

func WithFadeOutDuration(dur time.Duration) Options {
	return func(c config) config {
		c.fadeOutDuration = dur
		return c
	}
}
