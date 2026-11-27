package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/failsafe-go/failsafe-go/bulkhead"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
)

// routePrefixKey records the matched route prefix for metrics and
// incident reporting, since proxied requests have no gin route pattern.
const routePrefixKey = "route_prefix"

type ginContextKey struct{}

// Route maps one URL path prefix to an upstream service.
type Route struct {
	Prefix   string
	Upstream *url.URL
}

// RouteTable is the gateway's static routing configuration. It is parsed
// once at startup and is immutable afterwards, so reads need no locking.
type RouteTable struct {
	routes []Route
}

// ParseRoutes parses "prefix=upstream" pairs separated by commas, e.g.
//
//	/api/v1/users/=http://user-service:8090,/api/v1/orders/=http://order-service:8090
//
// Prefixes must start with "/" and upstreams must be absolute http(s) URLs.
// An empty spec is valid and yields a gateway that only serves probes.
func ParseRoutes(spec string) (*RouteTable, error) {
	table := &RouteTable{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, rawUpstream, found := strings.Cut(entry, "=")
		if !found || prefix == "" || rawUpstream == "" {
			return nil, fmt.Errorf("route %q must look like prefix=upstream", entry)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("route prefix %q must start with /", prefix)
		}
		upstream, err := url.Parse(rawUpstream)
		if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") {
			return nil, fmt.Errorf("route upstream %q must be an absolute http(s) URL", rawUpstream)
		}
		for _, existing := range table.routes {
			if existing.Prefix == prefix {
				return nil, fmt.Errorf("duplicate route prefix %q", prefix)
			}
		}
		table.routes = append(table.routes, Route{Prefix: prefix, Upstream: upstream})
	}
	// Longest prefix first so specific routes win over broad ones.
	sort.Slice(table.routes, func(i, j int) bool {
		return len(table.routes[i].Prefix) > len(table.routes[j].Prefix)
	})
	return table, nil
}

// Routes returns the configured routes in match order, for diagnostics.
func (t *RouteTable) Routes() []Route {
	return append([]Route(nil), t.routes...)
}

// Prefixes returns the route prefixes in match order.
func (t *RouteTable) Prefixes() []string {
	prefixes := make([]string, 0, len(t.routes))
	for _, route := range t.routes {
		prefixes = append(prefixes, route.Prefix)
	}
	return prefixes
}

func (t *RouteTable) match(path string) (Route, bool) {
	for _, route := range t.routes {
		if strings.HasPrefix(path, route.Prefix) {
			return route, true
		}
	}
	return Route{}, false
}

// ProxyOptions configures the upstream transport and resilience policies.
// Non-positive transport timeouts leave the corresponding http.Transport
// knob at its default, which keeps tests hermetic; production wiring always
// passes validated positive values from LoadConfig.
type ProxyOptions struct {
	Timeout               time.Duration
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	MaxConnsPerHost       int
	MaxRequestBodyBytes   int64
	RetryBodyBufferBytes  int64
	Resilience            ResilienceConfig
	Logger                *slog.Logger
	// TransportWrapper optionally wraps each route's base transport (used to
	// inject OpenTelemetry instrumentation). Nil means no wrapping.
	TransportWrapper func(http.RoundTripper) http.RoundTripper
}

// BuildResilienceSet composes the per-route resilience transports. Each
// route's base transport is wrapped by observingTransport so upstream calls
// are measured whether or not any resilience policy is enabled.
func (opts ProxyOptions) BuildResilienceSet(prefixes []string, metrics *Metrics) *ResilienceSet {
	resilience := opts.Resilience
	resilience.Metrics = metrics
	return NewResilienceSet(prefixes, resilience, func(prefix string) http.RoundTripper {
		return opts.baseTransport(prefix, metrics)
	})
}

func (opts ProxyOptions) baseTransport(route string, metrics *Metrics) http.RoundTripper {
	var transport http.RoundTripper = &observingTransport{
		route:   route,
		metrics: metrics,
		next:    opts.newUpstreamTransport(),
	}
	if opts.TransportWrapper != nil {
		transport = opts.TransportWrapper(transport)
	}
	return transport
}

func (opts ProxyOptions) newUpstreamTransport() *http.Transport {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   opts.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       opts.MaxConnsPerHost,
		ForceAttemptHTTP2:     true,
	}
	return transport
}

// ProxyHandler returns a terminal handler that forwards requests to the
// matched upstream, preserving the full request path. It is mounted as the
// router's NoRoute handler so any prefix can be served without route
// registration conflicts.
func (t *RouteTable) ProxyHandler(opts ProxyOptions, resilience *ResilienceSet) gin.HandlerFunc {
	proxies := make(map[string]*httputil.ReverseProxy, len(t.routes))
	for _, route := range t.routes {
		prefix := route.Prefix
		upstream := route.Upstream
		transport := resilience.RoundTripper(prefix)
		if transport == nil {
			transport = opts.baseTransport(prefix, opts.Resilience.Metrics)
		}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(upstream)
				r.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				upstreamErrorHandler(resilience, opts.Resilience.Metrics, prefix, w, r, err)
			},
			// The per-request context deadline bounds the whole upstream call,
			// including any retry attempts; per-attempt bounds come from the
			// transport's dial/TLS/response-header timeouts.
			Transport: transport,
		}
		proxies[prefix] = proxy
	}
	return func(c *gin.Context) {
		route, ok := t.match(c.Request.URL.Path)
		if !ok {
			abortWithError(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "no upstream route matches this path")
			return
		}
		c.Set(routePrefixKey, route.Prefix)
		if opts.MaxRequestBodyBytes > 0 && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, opts.MaxRequestBodyBytes)
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), opts.Timeout)
		defer cancel()
		ctx = context.WithValue(ctx, upstreamOutcomeKey{}, &upstreamOutcome{})
		if opts.Resilience.RetryMaxAttempts > 1 && retryMethodEligible(c.Request) {
			if prepareRetryBody(c.Request, opts.RetryBodyBufferBytes) {
				ctx = context.WithValue(ctx, retryEligibilityKey{}, true)
			}
		}
		ctx = context.WithValue(ctx, ginContextKey{}, c)
		c.Request = c.Request.WithContext(ctx)
		proxies[route.Prefix].ServeHTTP(c.Writer, c.Request)
	}
}

// retryMethodEligible reports whether replaying the request is safe: either
// the method is idempotent or the caller supplied an Idempotency-Key.
func retryMethodEligible(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return req.Header.Get("Idempotency-Key") != ""
}

// prepareRetryBody buffers a request body so retries can replay it. It
// returns false when the body cannot be buffered within limit; the request
// then continues unretryable with its body reassembled.
func prepareRetryBody(req *http.Request, limit int64) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		req.Body = struct {
			io.Reader
			io.Closer
		}{Reader: io.MultiReader(bytes.NewReader(data), req.Body), Closer: req.Body}
		return false
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return true
}

func upstreamErrorHandler(resilience *ResilienceSet, metrics *Metrics, prefix string, w http.ResponseWriter, r *http.Request, err error) {
	c, _ := r.Context().Value(ginContextKey{}).(*gin.Context)
	if c == nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.Is(err, circuitbreaker.ErrOpen):
		delay := resilience.BreakerRemainingDelay(prefix)
		c.Header("Retry-After", strconv.Itoa(max(1, int(math.Ceil(delay.Seconds())))))
		abortWithError(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "upstream is temporarily unavailable")
	case errors.Is(err, bulkhead.ErrFull):
		metrics.IncRejected(RejectReasonConcurrencyLimit)
		c.Set(gatewayIncidentTypeKey, incident.TypeConcurrencyLimit)
		c.Header("Retry-After", "1")
		abortWithError(c, http.StatusTooManyRequests, "CONCURRENCY_LIMIT", "too many in-flight requests to this upstream")
	case errors.As(err, &maxBytesErr):
		metrics.IncRejected(RejectReasonBodyTooLarge)
		abortWithError(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds the gateway limit")
	case isTimeoutError(err):
		c.Set(gatewayIncidentTypeKey, incident.TypeUpstreamTimeout)
		abortWithError(c, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "upstream request timed out")
	default:
		if !errors.Is(err, context.Canceled) {
			c.Set(gatewayIncidentTypeKey, incident.TypeUpstreamError)
			c.Set(gatewayIncidentDetailKey, truncateIncidentDetail(err.Error(), 256))
		}
		abortWithError(c, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
	}
}

func truncateIncidentDetail(raw string, limit int) string {
	raw = strings.TrimSpace(raw)
	if limit <= 0 || len(raw) <= limit {
		return raw
	}
	return raw[:limit]
}

// routeLabel returns the gin route pattern, or the matched proxy prefix for
// proxied requests, for metrics and incident reporting.
func routeLabel(c *gin.Context) string {
	if path := c.FullPath(); path != "" {
		return path
	}
	if value, ok := c.Get(routePrefixKey); ok {
		if prefix, ok := value.(string); ok {
			return prefix
		}
	}
	return "unmatched"
}
