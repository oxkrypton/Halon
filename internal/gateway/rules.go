package gateway

import (
	"errors"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// BlacklistStore is the contract behind blacklist checks and admin writes.
// Add/Remove report whether the entry actually changed, and store failures are
// surfaced as ErrBlacklistUnavailable so callers never pretend a write succeeded.
type BlacklistStore interface {
	Add(ip string) (created bool, err error)
	Remove(ip string) (removed bool, err error)
	Contains(ip string) bool
	List() []string
}

// ErrBlacklistUnavailable marks blacklist writes that failed because the
// backing store is unreachable. The admin API maps it to 503: a security
// feature must never appear to have succeeded when it has not.
var ErrBlacklistUnavailable = errors.New("blacklist store unavailable")

// BlacklistMiddleware blocks listed IPs. Each rejection invokes the optional
// hooks (used for metrics) before the error response is written.
func BlacklistMiddleware(store BlacklistStore, onRejected ...func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store.Contains(c.ClientIP()) {
			for _, hook := range onRejected {
				hook()
			}
			abortWithError(c, http.StatusForbidden, "IP_BLOCKED", "client IP is blocked")
			return
		}
		c.Next()
	}
}

func canonicalIP(value string) (string, error) {
	parsed := net.ParseIP(value)
	if parsed == nil {
		return "", errors.New("invalid IP address")
	}
	return parsed.String(), nil
}
