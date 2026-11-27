package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
)

// RateLimitBackend is the contract behind per-IP rate limiting.
// Update failures are surfaced as ErrRateLimitUnavailable so the admin API
// can answer 503.
type RateLimitBackend interface {
	Allow(ip string) bool
	Update(rps float64, burst int) error
	Settings() (rps float64, burst int)
}

// RateLimitIPStatus describes the read-only token bucket state of one client IP.
type RateLimitIPStatus struct {
	IP                string  `json:"ip"`
	Active            bool    `json:"active"`
	Tokens            float64 `json:"tokens"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
	TTLMS             int64   `json:"ttl_ms"`
}

// RateLimitInspector provides non-mutating inspection of per-IP bucket state.
type RateLimitInspector interface {
	InspectIP(ctx context.Context, ip string) (RateLimitIPStatus, error)
}

// ErrRateLimitUnavailable marks rate-limit config writes that failed
// because the backing store is unreachable.
var ErrRateLimitUnavailable = errors.New("rate limit store unavailable")

// RateLimitMiddleware applies the per-IP rate limit. Each rejection invokes
// the optional hooks (used for metrics) before the error response is written.
func RateLimitMiddleware(limiter RateLimitBackend, onRejected ...func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limiter.Allow(c.ClientIP()) {
			for _, hook := range onRejected {
				hook()
			}
			c.Set(gatewayIncidentTypeKey, incident.TypeRateLimitAbuse)
			c.Header("Retry-After", "1")
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			return
		}
		c.Next()
	}
}
