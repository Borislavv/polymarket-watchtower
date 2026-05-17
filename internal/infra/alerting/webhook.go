package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

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
