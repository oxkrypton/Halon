package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
)

func gatewayStateHandler(rules BlacklistStore, limiter RateLimitBackend, resilience *ResilienceSet, metrics *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		ips := []string{}
		if rules != nil {
			ips = rules.List()
		}
		var rps float64
		var burst int
		if limiter != nil {
			rps, burst = limiter.Settings()
		}
		breakers := []BreakerStatus{}
		if resilience != nil {
			breakers = resilience.BreakerStatuses()
		}
		redisUp := true
		if metrics != nil {
			redisUp = metrics.RedisUp()
		}
		c.JSON(http.StatusOK, gin.H{
			"redis_up": redisUp,
			"blacklist": gin.H{
				"count": len(ips),
				"ips":   ips,
			},
			"rate_limit": gin.H{
				"requests_per_second": rps,
				"burst":               burst,
			},
			"breakers": breakers,
		})
	}
}

func rateLimitIPStateHandler(rules BlacklistStore, limiter RateLimitBackend, metrics *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		canonical, err := canonicalIP(c.Param("ip"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_IP", err.Error())
			return
		}
		inBlacklist := false
		if rules != nil {
			inBlacklist = rules.Contains(canonical)
		}
		status := RateLimitIPStatus{IP: canonical}
		if inspector, ok := limiter.(RateLimitInspector); ok {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			inspected, inspectErr := inspector.InspectIP(ctx, canonical)
			if inspectErr != nil {
				if errors.Is(inspectErr, ErrRateLimitUnavailable) {
					abortWithError(c, http.StatusServiceUnavailable, "RATELIMIT_UNAVAILABLE", "rate limit store is unavailable")
					return
				}
				abortWithError(c, http.StatusInternalServerError, "STATE_QUERY_FAILED", "failed to inspect rate limit state")
				return
			}
			status = inspected
		} else if limiter != nil {
			rps, burst := limiter.Settings()
			status.RequestsPerSecond = rps
			status.Burst = burst
			status.Tokens = float64(burst)
		}
		redisUp := true
		if metrics != nil {
			redisUp = metrics.RedisUp()
		}
		c.JSON(http.StatusOK, gin.H{
			"ip":                  status.IP,
			"in_blacklist":        inBlacklist,
			"active":              status.Active,
			"tokens":              status.Tokens,
			"requests_per_second": status.RequestsPerSecond,
			"burst":               status.Burst,
			"ttl_ms":              status.TTLMS,
			"redis_up":            redisUp,
		})
	}
}

func incidentWindowsStateHandler(coalescer IncidentCoalescer, metrics *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		var filterType incident.EventType
		if rawType := strings.TrimSpace(c.Query("type")); rawType != "" {
			parsed, err := incident.ParseEventType(rawType)
			if err != nil {
				abortWithError(c, http.StatusBadRequest, "INVALID_EVENT_TYPE", err.Error())
				return
			}
			filterType = parsed
		}
		filterScope := strings.TrimSpace(c.Query("scope"))
		if filterScope == "" {
			filterScope = strings.TrimSpace(c.Query("route"))
		}

		windows := []IncidentWindowSnapshot{}
		if coalescer != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			defer cancel()
			list, err := coalescer.ListWindows(ctx, filterType, filterScope)
			if err != nil && len(list) == 0 {
				abortWithError(c, http.StatusServiceUnavailable, "STATE_UNAVAILABLE", "incident coalescing state is unavailable")
				return
			}
			windows = list
		}
		redisUp := true
		if metrics != nil {
			redisUp = metrics.RedisUp()
		}
		c.JSON(http.StatusOK, gin.H{
			"windows":  windows,
			"redis_up": redisUp,
		})
	}
}
