package alerting

import (
	"context"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/rs/zerolog"
)

// LogSink writes findings to the zerolog logger.
type LogSink struct {
	Logger *zerolog.Logger
}

func (s *LogSink) Name() string { return "log" }

func (s *LogSink) Notify(_ context.Context, f anomaly.Finding) error {
	evt := s.Logger.Warn()
	if f.Severity == anomaly.SeverityCritical || f.Severity == anomaly.SeverityFatal {
		evt = s.Logger.Error()
	}
	evt.
		Str("scope", string(f.Scope)).
		Str("market", string(f.Market)).
		Int64("category", int64(f.Category)).
		Str("label", f.Label).
		Str("metric", string(f.Metric)).
		Str("severity", string(f.Severity)).
		Float64("multiplier", f.Multiplier).
		Float64("recent", f.Recent).
		Float64("baseline", f.Baseline).
		Dur("window", f.WindowLen).
		Dur("baseline_window", f.BaselineLen).
		Time("at", f.At).
		Msg("anomaly detected")
	return nil
}
