package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

type TimeoutNotifier struct {
	webhookURL string
	token      string
	queue      <-chan incident.TimeoutEvent
	client     *http.Client
	logger     *slog.Logger
	metrics    *Metrics
}

func NewTimeoutNotifier(webhookURL, token string, queue <-chan incident.TimeoutEvent, logger *slog.Logger, metrics *Metrics) *TimeoutNotifier {
	return &TimeoutNotifier{
		webhookURL: webhookURL,
		token:      token,
		queue:      queue,
		client:     &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
		metrics:    metrics,
	}
}

func (n *TimeoutNotifier) Run() {
	for event := range n.queue {
		if err := n.send(event); err != nil {
			n.metrics.IncWebhookNotifyFailures()
			n.logger.Error("deliver timeout incident", "event_id", event.EventID, "error", err)
			continue
		}
		n.logger.Info("timeout incident delivered", "event_id", event.EventID, "path", event.Path)
	}
}

func (n *TimeoutNotifier) send(event incident.TimeoutEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	var lastError error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(payload))
		if err != nil {
			cancel()
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+n.token)
		response, err := n.client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				cancel()
				return nil
			}
			err = fmt.Errorf("webhook returned %s", response.Status)
			if response.StatusCode < 500 {
				cancel()
				return err
			}
		}
		cancel()
		lastError = err
		if attempt < 2 {
			time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
		}
	}
	return lastError
}
