package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

type Pinger interface {
	Ping(context.Context) error
}

type Dependencies struct {
	Routes               *RouteTable
	Repository           Pinger
	LogQueue             chan<- trafficlog.Entry
	TimeoutQueue         chan<- incident.TimeoutEvent
	Logger               *slog.Logger
	Rules                BlacklistStore
	Limiter              *RateLimiter
	Metrics              *Metrics
	Proxy                ProxyOptions
	Resilience           *ResilienceSet
	SlowRequestThreshold time.Duration
	BodyCaptureLimit     int64
}

func NewHTTPHandler(dependencies Dependencies) *gin.Engine {
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	metrics := dependencies.Metrics
	router := newRouter(logger, AccessLog(AccessLogOptions{
		Queue:         dependencies.LogQueue,
		TimeoutQueue:  dependencies.TimeoutQueue,
		SlowThreshold: dependencies.SlowRequestThreshold,
		CaptureLimit:  dependencies.BodyCaptureLimit,
		Logger:        logger,
		Metrics:       metrics,
	}))

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", readinessHandler(dependencies.Repository))

	// Everything that is not a probe is a proxy candidate: blacklist and
	// rate limit first, then the route table decides the upstream.
	routes := dependencies.Routes
	if routes == nil {
		routes = &RouteTable{}
	}
	proxy := dependencies.Proxy
	proxy.Resilience.Metrics = metrics
	proxyMiddlewares := []gin.HandlerFunc{
		BlacklistMiddleware(dependencies.Rules, func() { metrics.IncRejected(RejectReasonBlacklist) }),
		dependencies.Limiter.Middleware(func() { metrics.IncRejected(RejectReasonRateLimit) }),
		routes.ProxyHandler(proxy, dependencies.Resilience),
	}
	router.NoRoute(func(c *gin.Context) {
		for _, middleware := range proxyMiddlewares {
			middleware(c)
			if c.IsAborted() {
				return
			}
		}
	})
	return router
}

// AdminOptions configures optional admin surface features.
type AdminOptions struct {
	MetricsRequireAuth bool
	Resilience         *ResilienceSet
}

func NewAdminHandler(token string, rules BlacklistStore, limiter *RateLimiter, logger *slog.Logger, metrics *Metrics, options AdminOptions) *gin.Engine {
	router := newRouter(logger)
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	if metrics != nil {
		metricsHandler := gin.WrapH(metrics.Handler())
		if options.MetricsRequireAuth {
			router.GET("/metrics", adminAuthentication(token), metricsHandler)
		} else {
			router.GET("/metrics", metricsHandler)
		}
	}

	admin := router.Group("/admin", adminAuthentication(token))
	if options.Resilience != nil {
		admin.GET("/breakers", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"breakers": options.Resilience.BreakerStatuses()})
		})
	}
	admin.GET("/rules/blacklist", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ips": rules.List()})
	})
	admin.PUT("/rules/blacklist/:ip", func(c *gin.Context) {
		created, err := rules.Add(c.Param("ip"))
		if err != nil {
			if errors.Is(err, ErrBlacklistUnavailable) {
				abortWithError(c, http.StatusServiceUnavailable, "BLACKLIST_UNAVAILABLE", "blacklist store is unavailable")
				return
			}
			abortWithError(c, http.StatusBadRequest, "INVALID_IP", err.Error())
			return
		}
		statusCode := http.StatusOK
		if created {
			statusCode = http.StatusCreated
		}
		body := gin.H{"ip": c.Param("ip")}
		if propagator, ok := rules.(interface{ Propagation() string }); ok {
			body["propagation"] = propagator.Propagation()
		}
		c.JSON(statusCode, body)
	})
	admin.DELETE("/rules/blacklist/:ip", func(c *gin.Context) {
		removed, err := rules.Remove(c.Param("ip"))
		if err != nil {
			if errors.Is(err, ErrBlacklistUnavailable) {
				abortWithError(c, http.StatusServiceUnavailable, "BLACKLIST_UNAVAILABLE", "blacklist store is unavailable")
				return
			}
			abortWithError(c, http.StatusBadRequest, "INVALID_IP", err.Error())
			return
		}
		if !removed {
			abortWithError(c, http.StatusNotFound, "RULE_NOT_FOUND", "IP is not blocked")
			return
		}
		c.Status(http.StatusNoContent)
	})
	admin.GET("/rules/rate-limit", func(c *gin.Context) {
		rps, burst := limiter.Settings()
		c.JSON(http.StatusOK, gin.H{"requests_per_second": rps, "burst": burst})
	})
	admin.PUT("/rules/rate-limit", func(c *gin.Context) {
		var request struct {
			RequestsPerSecond float64 `json:"requests_per_second" binding:"required,gt=0"`
			Burst             int     `json:"burst" binding:"required,gt=0"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_RATE_LIMIT", "positive requests_per_second and burst are required")
			return
		}
		limiter.Update(request.RequestsPerSecond, request.Burst)
		c.JSON(http.StatusOK, gin.H{"requests_per_second": request.RequestsPerSecond, "burst": request.Burst})
	})
	return router
}

func newRouter(logger *slog.Logger, middleware ...gin.HandlerFunc) *gin.Engine {
	if logger == nil {
		logger = slog.Default()
	}
	router := gin.New()
	// Do not trust forwarding headers until a trusted reverse proxy is deployed.
	if err := router.SetTrustedProxies(nil); err != nil {
		panic(err)
	}
	router.HandleMethodNotAllowed = false
	router.Use(RequestID())
	if len(middleware) > 0 {
		router.Use(middleware...)
	}
	router.Use(Recovery(logger))
	router.NoMethod(func(c *gin.Context) {
		abortWithError(c, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})
	return router
}

func readinessHandler(repository Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if repository.Ping(ctx) != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"checks": gin.H{"database": false},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
			"checks": gin.H{"database": true},
		})
	}
}
