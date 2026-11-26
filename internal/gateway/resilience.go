package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/bulkhead"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/failsafehttp"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

// ResilienceConfig carries the per-route resilience policy settings. All
// policies are in-memory, matching the single-replica boundary documented in
// AGENTS.md.
type ResilienceConfig struct {
	RetryMaxAttempts         int
	BreakerEnabled           bool
	BreakerFailureThreshold  uint
	BreakerOpenDuration      time.Duration
	BreakerHalfOpenSuccesses uint
	BreakerCount5xx          bool
	ConcurrencyPerRoute      uint
	Metrics                  *Metrics
	Logger                   *slog.Logger
}

// upstreamOutcome records the final upstream call classification of one
// request so the access log can report it. Writes happen on the proxy handler
// goroutine before the access log reads it, so no locking is needed.
type upstreamOutcome struct {
	result          string
	retries         int
	upstreamLatency time.Duration
}

type upstreamOutcomeKey struct{}

// retryEligibilityKey marks requests whose method is idempotent and whose
// body has been buffered for replay. Only eligible requests take the retry
// policy path, so retries never replay an unbounded body.
type retryEligibilityKey struct{}

type routeResilience struct {
	breaker      circuitbreaker.CircuitBreaker[*http.Response]
	withRetry    http.RoundTripper
	withoutRetry http.RoundTripper
}

func (r *routeResilience) roundTripper() http.RoundTripper {
	if r.withRetry == nil {
		return r.withoutRetry
	}
	return http.RoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if eligible, _ := req.Context().Value(retryEligibilityKey{}).(bool); eligible {
			return r.withRetry.RoundTrip(req)
		}
		return r.withoutRetry.RoundTrip(req)
	}))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// ResilienceSet holds the composed resilience transports for every route
// prefix. It is built once at startup; policies are concurrency safe.
type ResilienceSet struct {
	routes map[string]*routeResilience
}

// NewResilienceSet composes bulkhead → circuit breaker → retry around each
// route's base transport (outermost first, matching failsafehttp semantics).
// baseFor is called once per route prefix to supply the route's transport.
func NewResilienceSet(prefixes []string, cfg ResilienceConfig, baseFor func(prefix string) http.RoundTripper) *ResilienceSet {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	set := &ResilienceSet{routes: make(map[string]*routeResilience, len(prefixes))}
	for _, prefix := range prefixes {
		base := baseFor(prefix)
		entry := &routeResilience{}
		policies := make([]failsafe.Policy[*http.Response], 0, 3)

		if cfg.ConcurrencyPerRoute > 0 {
			route := prefix
			policies = append(policies, bulkhead.NewBuilder[*http.Response](cfg.ConcurrencyPerRoute).
				OnAcquired(func(failsafe.ExecutionEvent[*http.Response]) { cfg.Metrics.IncUpstreamInFlight(route) }).
				OnReleased(func(failsafe.ExecutionEvent[*http.Response]) { cfg.Metrics.DecUpstreamInFlight(route) }).
				Build())
		}

		if cfg.BreakerEnabled {
			route := prefix
			breaker := circuitbreaker.NewBuilder[*http.Response]().
				HandleIf(func(resp *http.Response, err error) bool {
					if err != nil {
						// Client disconnects say nothing about upstream health.
						return !errors.Is(err, context.Canceled)
					}
					return cfg.BreakerCount5xx && resp != nil && resp.StatusCode >= http.StatusInternalServerError
				}).
				WithFailureThreshold(cfg.BreakerFailureThreshold).
				WithSuccessThreshold(cfg.BreakerHalfOpenSuccesses).
				WithDelay(cfg.BreakerOpenDuration).
				OnOpen(func(circuitbreaker.StateChangedEvent) {
					cfg.Metrics.IncBreakerTrips(route)
					cfg.Metrics.SetBreakerState(route, breakerStateOpen)
					logger.Warn("circuit breaker open", "route", route)
				}).
				OnHalfOpen(func(circuitbreaker.StateChangedEvent) {
					cfg.Metrics.SetBreakerState(route, breakerStateHalfOpen)
					logger.Info("circuit breaker half-open", "route", route)
				}).
				OnClose(func(circuitbreaker.StateChangedEvent) {
					cfg.Metrics.SetBreakerState(route, breakerStateClosed)
					logger.Info("circuit breaker closed", "route", route)
				}).
				Build()
			entry.breaker = breaker
			cfg.Metrics.SetBreakerState(route, breakerStateClosed)
			policies = append(policies, breaker)
		}

		if len(policies) == 0 {
			entry.withoutRetry = base
		} else {
			entry.withoutRetry = failsafehttp.NewRoundTripper(base, policies...)
		}

		if cfg.RetryMaxAttempts > 1 {
			route := prefix
			retry := retrypolicy.NewBuilder[*http.Response]().
				// Transport-level errors only: nothing was written to the
				// client yet, so replaying the attempt is always safe.
				HandleIf(func(_ *http.Response, err error) bool { return err != nil }).
				AbortOnErrors(circuitbreaker.ErrOpen, context.Canceled).
				WithMaxAttempts(cfg.RetryMaxAttempts).
				WithBackoff(50*time.Millisecond, 500*time.Millisecond).
				WithJitter(50 * time.Millisecond).
				ReturnLastFailure().
				OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[*http.Response]) {
					cfg.Metrics.IncRetry(route)
					if outcome, ok := event.Context().Value(upstreamOutcomeKey{}).(*upstreamOutcome); ok {
						outcome.retries++
					}
				}).
				OnRetriesExceeded(func(failsafe.ExecutionEvent[*http.Response]) {
					cfg.Metrics.IncRetryExhausted(route)
				}).
				Build()
			entry.withRetry = failsafehttp.NewRoundTripper(base, append(policies, retry)...)
		}
		set.routes[prefix] = entry
	}
	return set
}

// RoundTripper returns the resilience-composing transport for a route prefix.
func (s *ResilienceSet) RoundTripper(prefix string) http.RoundTripper {
	if s == nil {
		return nil
	}
	entry, ok := s.routes[prefix]
	if !ok {
		return nil
	}
	return entry.roundTripper()
}

// BreakerRemainingDelay reports how long the route's breaker stays open; it
// returns 0 when the breaker is closed or absent.
func (s *ResilienceSet) BreakerRemainingDelay(prefix string) time.Duration {
	if s == nil {
		return 0
	}
	if entry, ok := s.routes[prefix]; ok && entry.breaker != nil {
		return entry.breaker.RemainingDelay()
	}
	return 0
}

// BreakerStatus is one route's circuit breaker snapshot for diagnostics.
type BreakerStatus struct {
	Route                 string  `json:"route"`
	State                 string  `json:"state"`
	RemainingDelaySeconds float64 `json:"remaining_delay_seconds,omitempty"`
}

// BreakerStatuses returns breaker snapshots for all routes, sorted for
// stable output. Routes without a breaker report state "disabled".
func (s *ResilienceSet) BreakerStatuses() []BreakerStatus {
	statuses := make([]BreakerStatus, 0, len(s.routes))
	for prefix, entry := range s.routes {
		status := BreakerStatus{Route: prefix, State: "disabled"}
		if entry.breaker != nil {
			switch entry.breaker.State() {
			case circuitbreaker.OpenState:
				status.State = "open"
				status.RemainingDelaySeconds = entry.breaker.RemainingDelay().Seconds()
			case circuitbreaker.HalfOpenState:
				status.State = "half-open"
			default:
				status.State = "closed"
			}
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Route < statuses[j].Route })
	return statuses
}

// isTimeoutError reports whether err means the upstream did not answer in
// time: either the overall request deadline or a transport-level net timeout
// (dial, TLS handshake, response header) fired.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// observingTransport wraps a route's base transport to record upstream call
// metrics and the per-request outcome consumed by the access log.
type observingTransport struct {
	route   string
	metrics *Metrics
	next    http.RoundTripper
}

func (t *observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.next.RoundTrip(req)
	result := UpstreamResultSuccess
	if err != nil {
		if isTimeoutError(err) {
			result = UpstreamResultTimeout
		} else {
			result = UpstreamResultError
		}
	}
	elapsed := time.Since(started)
	t.metrics.ObserveUpstream(t.route, result, elapsed)
	if outcome, ok := req.Context().Value(upstreamOutcomeKey{}).(*upstreamOutcome); ok {
		outcome.result = result
		outcome.upstreamLatency += elapsed
	}
	return resp, err
}
