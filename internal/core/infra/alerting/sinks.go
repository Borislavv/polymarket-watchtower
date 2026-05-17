// Package alerting fans-out anomaly findings to one or more sinks.
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/core/domain/anomaly"
	"github.com/rs/zerolog"
)

// Sink consumes findings. Implementations must be safe for concurrent use.
type Sink interface {
	Notify(ctx context.Context, f anomaly.Finding) error
	Name() string
}

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

// WebhookSink POSTs a JSON payload to the configured URL.
type WebhookSink struct {
	URL    string
	Client *http.Client
}

func NewWebhookSink(url string) *WebhookSink {
	return &WebhookSink{URL: url, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (s *WebhookSink) Name() string { return "webhook" }

func (s *WebhookSink) Notify(ctx context.Context, f anomaly.Finding) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return nil
}

// Fanout broadcasts to multiple sinks; errors are logged but never block other
// sinks.
type Fanout struct {
	Sinks  []Sink
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
