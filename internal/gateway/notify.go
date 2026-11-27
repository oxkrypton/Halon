package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

// IncidentNotifierOptions configures cross-replica coalescing, threshold burst
// policies, and delivery cooldown windows for IncidentNotifier.
type IncidentNotifierOptions struct {
	Coalescer IncidentCoalescer
	Policies  IncidentPolicySet
}

type pendingCoalescedEvent struct {
	event  incident.Event
	policy IncidentPolicy
	timer  *time.Timer
}

// IncidentNotifier consumes raw gateway anomaly events, coalesces and filters
// them via IncidentCoalescer, waits a bounded settle delay, and delivers
// enriched incident webhooks to the Agent service.
type IncidentNotifier struct {
	webhookURL string
	token      string
	queue      <-chan incident.Event
	client     *http.Client
	logger     *slog.Logger
	metrics    *Metrics
	coalescer  IncidentCoalescer
	policies   IncidentPolicySet

	mu      sync.Mutex
	closed  bool
	pending map[string]*pendingCoalescedEvent
	wg      sync.WaitGroup
}

func NewIncidentNotifier(webhookURL, token string, queue <-chan incident.Event, logger *slog.Logger, metrics *Metrics) *IncidentNotifier {
	return NewIncidentNotifierWithOptions(webhookURL, token, queue, logger, metrics, IncidentNotifierOptions{})
}

func NewIncidentNotifierWithOptions(webhookURL, token string, queue <-chan incident.Event, logger *slog.Logger, metrics *Metrics, options IncidentNotifierOptions) *IncidentNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &IncidentNotifier{
		webhookURL: webhookURL,
		token:      token,
		queue:      queue,
		client:     &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
		metrics:    metrics,
		coalescer:  options.Coalescer,
		policies:   options.Policies,
		pending:    make(map[string]*pendingCoalescedEvent),
	}
}

func (n *IncidentNotifier) Run() {
	for event := range n.queue {
		policy := n.policies.PolicyFor(event.Type)
		if !policy.Enabled {
			continue
		}
		event.ScopeKey = event.EffectiveScopeKey()
		useCoalescer := n.coalescer != nil && (policy.MinOccurrences > 1 || policy.Cooldown > 0)
		if !useCoalescer {
			n.emit(event, policy, false)
			continue
		}

		res := n.coalescer.Record(context.Background(), event, policy)
		if !res.Triggered {
			if res.Status == WindowActive {
				n.metrics.IncIncidentEventsCoalesced(event.Type, event.Route)
				n.logger.Debug("incident event coalesced into active window",
					"event_id", event.EventID, "owner_event_id", res.OwnerEventID,
					"event_type", event.Type, "scope_key", event.ScopeKey)
			}
			continue
		}

		if res.OwnerEventID != "" {
			event.EventID = res.OwnerEventID
		}
		if policy.SettleDelay <= 0 {
			n.wg.Add(1)
			go func(evt incident.Event, pol IncidentPolicy) {
				defer n.wg.Done()
				n.emit(evt, pol, true)
			}(event, policy)
			continue
		}
		n.schedulePending(event, policy)
	}

	n.mu.Lock()
	n.closed = true
	flushed := make([]*pendingCoalescedEvent, 0, len(n.pending))
	for id, p := range n.pending {
		if p.timer.Stop() {
			flushed = append(flushed, p)
			delete(n.pending, id)
			n.wg.Done()
		}
	}
	n.mu.Unlock()

	for _, item := range flushed {
		n.emit(item.event, item.policy, true)
	}
	n.wg.Wait()
}

func (n *IncidentNotifier) schedulePending(event incident.Event, policy IncidentPolicy) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.emit(event, policy, true)
		}()
		return
	}
	n.wg.Add(1)
	p := &pendingCoalescedEvent{event: event, policy: policy}
	n.pending[event.EventID] = p
	p.timer = time.AfterFunc(policy.SettleDelay, func() {
		defer n.wg.Done()
		n.mu.Lock()
		item, ok := n.pending[event.EventID]
		if !ok {
			n.mu.Unlock()
			return
		}
		delete(n.pending, event.EventID)
		n.mu.Unlock()
		n.emit(item.event, item.policy, true)
	})
}

func (n *IncidentNotifier) emit(event incident.Event, policy IncidentPolicy, coalesced bool) {
	scopeKey := event.EffectiveScopeKey()
	event.ScopeKey = scopeKey
	if coalesced && n.coalescer != nil {
		if snap, ok := n.coalescer.Snapshot(context.Background(), event.Type, scopeKey, event.EventID); ok {
			event.Occurrences = snap.Occurrences
			event.WindowSeconds = int(policy.SettleDelay.Seconds())
			if policy.MinOccurrences > 1 && policy.DetectWindow > policy.SettleDelay {
				event.WindowSeconds = int(policy.DetectWindow.Seconds())
			}
			event.SamplePaths = snap.SamplePaths
			event.SampleClientIPs = snap.SampleClientIPs
		} else if event.Occurrences <= 0 {
			event.Occurrences = 1
		}
	} else if event.Occurrences <= 0 {
		event.Occurrences = 1
	}
	if event.Summary == "" {
		event.Summary = fmt.Sprintf("%s on %s (%d occurrence(s))", event.Type, scopeKey, event.Occurrences)
	}

	if err := n.send(event); err != nil {
		n.metrics.IncWebhookNotifyFailures()
		n.logger.Error("deliver incident webhook",
			"event_id", event.EventID, "event_type", event.Type, "scope_key", scopeKey,
			"occurrences", event.Occurrences, "error", err)
		if coalesced && n.coalescer != nil {
			backoff := policy.RetryBackoff
			if policy.Cooldown > 0 && backoff > policy.Cooldown {
				backoff = policy.Cooldown
			}
			n.coalescer.UpdateCooldown(context.Background(), event.Type, scopeKey, event.EventID, backoff)
		}
		return
	}

	n.metrics.IncIncidentsEmitted(event.Type, event.Route)
	n.logger.Info("incident webhook delivered",
		"event_id", event.EventID, "event_type", event.Type, "scope_key", scopeKey,
		"path", event.Path, "occurrences", event.Occurrences)
	if coalesced && n.coalescer != nil {
		n.coalescer.UpdateCooldown(context.Background(), event.Type, scopeKey, event.EventID, policy.Cooldown)
	}
}

func (n *IncidentNotifier) send(event incident.Event) error {
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
