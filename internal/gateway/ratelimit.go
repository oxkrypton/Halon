package gateway

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// RateLimitBackend is the contract behind per-IP rate limiting.
// Update failures are surfaced as ErrRateLimitUnavailable so the admin API
// can answer 503.
type RateLimitBackend interface {
	Allow(ip string) bool
	Update(rps float64, burst int) error
	Settings() (rps float64, burst int)
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
			c.Header("Retry-After", "1")
			abortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			return
		}
		c.Next()
	}
}
