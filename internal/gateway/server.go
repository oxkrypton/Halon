package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
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
	Limiter              RateLimitBackend
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
		RateLimitMiddleware(dependencies.Limiter, func() { metrics.IncRejected(RejectReasonRateLimit) }),
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
	// Auth is normally supplied from Config.Auth. Nil keeps the legacy
	// single-token behaviour, which existing tests and callers rely on.
	Auth *authz.Authenticator
	// Local login is opt-in. Both LoginEnabled and AuthService must be set
	// for /auth/* to be served; otherwise those paths answer LOGIN_DISABLED.
	LoginEnabled bool
	AuthService  AuthService
	UserService  UserService
	LoginLimiter RateLimitBackend

	// Console observability, route inspection, and distributed trace query dependencies.
	TrafficLogs TrafficLogReader
	Routes      *RouteTable
	Tempo       *TempoClient
	SystemInfo  SystemInfo
}

func NewAdminHandler(token string, rules BlacklistStore, limiter RateLimitBackend, logger *slog.Logger, metrics *Metrics, options AdminOptions) *gin.Engine {
	router := newRouter(logger)
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	authenticator := options.Auth
	if authenticator == nil {
		// Legacy fallback: the original ADMIN_TOKEN maps to a full principal.
		authenticator, _ = authz.NewAuthenticator(
			authz.Config{Mode: authz.ModeLegacy, Issuer: authz.DefaultIssuer},
			authz.AudienceGatewayAdmin, token,
		)
	}
	adminMiddlewares := []gin.HandlerFunc{
		auditAdminRequest(logger, metrics),
		authz.Middleware(authenticator, abortWithError),
	}
	if metrics != nil {
		metricsHandler := gin.WrapH(metrics.Handler())
		if options.MetricsRequireAuth {
			// Scrapes are high-frequency: authenticate and authorize them,
			// but never write an audit log entry (or admin metric) per scrape.
			// Build a fresh chain instead of appending to adminMiddlewares so
			// the audit middleware cannot leak into this route.
			metricsMiddlewares := slices.Concat(
				[]gin.HandlerFunc{
					authz.Middleware(authenticator, abortWithError),
					authz.Require(authz.PermGatewayMetricsRead, abortWithError),
				},
				[]gin.HandlerFunc{metricsHandler},
			)
			router.GET("/metrics", metricsMiddlewares...)
		} else {
			router.GET("/metrics", metricsHandler)
		}
	}

	admin := router.Group("/admin", adminMiddlewares...)
	if options.Resilience != nil {
		admin.GET("/breakers", authz.Require(authz.PermGatewayBreakerRead, abortWithError), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"breakers": options.Resilience.BreakerStatuses()})
		})
	}
	admin.GET("/rules/blacklist", authz.Require(authz.PermGatewayRulesRead, abortWithError), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ips": rules.List()})
	})
	admin.PUT("/rules/blacklist/:ip", authz.Require(authz.PermGatewayBlacklistWrite, abortWithError), func(c *gin.Context) {
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
	admin.DELETE("/rules/blacklist/:ip", authz.Require(authz.PermGatewayBlacklistWrite, abortWithError), func(c *gin.Context) {
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
	admin.GET("/rules/rate-limit", authz.Require(authz.PermGatewayRulesRead, abortWithError), func(c *gin.Context) {
		rps, burst := limiter.Settings()
		c.JSON(http.StatusOK, gin.H{"requests_per_second": rps, "burst": burst})
	})
	admin.PUT("/rules/rate-limit", authz.Require(authz.PermGatewayRateLimitWrite, abortWithError), func(c *gin.Context) {
		var request struct {
			RequestsPerSecond float64 `json:"requests_per_second" binding:"required,gt=0"`
			Burst             int     `json:"burst" binding:"required,gt=0"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_RATE_LIMIT", "positive requests_per_second and burst are required")
			return
		}
		if err := limiter.Update(request.RequestsPerSecond, request.Burst); err != nil {
			if errors.Is(err, ErrRateLimitUnavailable) {
				abortWithError(c, http.StatusServiceUnavailable, "RATELIMIT_UNAVAILABLE", "rate limit store is unavailable")
				return
			}
			abortWithError(c, http.StatusBadRequest, "INVALID_RATE_LIMIT", err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"requests_per_second": request.RequestsPerSecond, "burst": request.Burst})
	})

	obsRead := authz.Require(authz.PermGatewayObservabilityRead, abortWithError)
	admin.GET("/system", obsRead, systemInfoHandler(options.Routes, options.SystemInfo, metrics))
	admin.GET("/observability/overview", obsRead, observabilityOverviewHandler(options.TrafficLogs))
	admin.GET("/observability/timeseries", obsRead, observabilityTimeSeriesHandler(options.TrafficLogs))
	admin.GET("/observability/routes", obsRead, observabilityRoutesHandler(options.TrafficLogs))
	admin.GET("/observability/logs", obsRead, observabilityLogsHandler(options.TrafficLogs))
	admin.GET("/observability/logs/:id", obsRead, observabilityLogDetailHandler(options.TrafficLogs))
	admin.GET("/observability/top-ips", obsRead, observabilityTopIPsHandler(options.TrafficLogs))
	admin.GET("/traces", obsRead, listTracesHandler(options.Tempo, options.TrafficLogs))
	admin.GET("/traces/:trace_id", obsRead, getTraceHandler(options.Tempo, options.TrafficLogs))

	if options.LoginEnabled && options.AuthService != nil {
		registerAuthRoutes(router, authenticator, options.AuthService, options.LoginLimiter, logger)
	} else {
		registerAuthDisabledRoutes(router)
	}
	if options.LoginEnabled && options.UserService != nil {
		registerUserRoutes(router, authenticator, options.UserService, logger, metrics)
	} else {
		registerUserDisabledRoutes(router)
	}
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

// auditAdminRequest records every authenticated admin call, including
// authentication and permission failures, without adding a database table.
func auditAdminRequest(logger *slog.Logger, metrics *Metrics) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		c.Next()
		principal := authz.PrincipalFromContext(c)
		role, subject, typ, jti := "unknown", "", "", ""
		if principal != nil {
			role = principal.Role.String()
			subject = principal.Subject
			typ = string(principal.Type)
			jti = principal.JTI
		}
		permission := authz.PermissionFromContext(c).String()
		result := "allow"
		if authz.DeniedFromContext(c) {
			result = "deny"
		}
		logger.Info("admin request",
			"sub", subject,
			"typ", typ,
			"role", role,
			"jti", jti,
			"permission", permission,
			"result", result,
			"status", c.Writer.Status(),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"request_id", requestIDFromContext(c),
		)
		if metrics != nil {
			metrics.IncAdminRequest(role, permission, result)
		}
	}
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
