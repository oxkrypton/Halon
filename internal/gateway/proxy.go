package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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

func (t *RouteTable) match(path string) (Route, bool) {
	for _, route := range t.routes {
		if strings.HasPrefix(path, route.Prefix) {
			return route, true
		}
	}
	return Route{}, false
}

// ProxyHandler returns a terminal handler that forwards requests to the
// matched upstream, preserving the full request path. It is mounted as the
// router's NoRoute handler so any prefix can be served without route
// registration conflicts.
func (t *RouteTable) ProxyHandler(timeout time.Duration) gin.HandlerFunc {
	proxies := make(map[string]*httputil.ReverseProxy, len(t.routes))
	for _, route := range t.routes {
		upstream := route.Upstream
		proxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(upstream)
				r.SetXForwarded()
			},
			ErrorHandler: upstreamErrorHandler,
			// The per-request context deadline already bounds the upstream call.
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		}
		proxies[route.Prefix] = proxy
	}
	return func(c *gin.Context) {
		route, ok := t.match(c.Request.URL.Path)
		if !ok {
			abortWithError(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "no upstream route matches this path")
			return
		}
		c.Set(routePrefixKey, route.Prefix)
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		ctx = context.WithValue(ctx, ginContextKey{}, c)
		c.Request = c.Request.WithContext(ctx)
		proxies[route.Prefix].ServeHTTP(c.Writer, c.Request)
	}
}

func upstreamErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	c, _ := r.Context().Value(ginContextKey{}).(*gin.Context)
	if c == nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.Set(upstreamTimeoutKey, true)
		abortWithError(c, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "upstream request timed out")
		return
	}
	abortWithError(c, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
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
