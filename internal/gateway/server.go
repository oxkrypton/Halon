package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"github.com/oxkrypton/Halon/internal/trafficlog"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type HealthChecker interface {
	Check(context.Context, *healthpb.HealthCheckRequest, ...grpc.CallOption) (*healthpb.HealthCheckResponse, error)
}

type Pinger interface {
	Ping(context.Context) error
}

type Dependencies struct {
	UserClient           userv1.UserServiceClient
	HealthClient         HealthChecker
	Repository           Pinger
	LogQueue             chan<- trafficlog.Entry
	Logger               *slog.Logger
	Rules                *IPRules
	Limiter              *RateLimiter
	RPCTimeout           time.Duration
	SlowRequestThreshold time.Duration
	BodyCaptureLimit     int64
}

func NewHTTPHandler(dependencies Dependencies) *gin.Engine {
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	router := newRouter(logger, AccessLog(dependencies.LogQueue, dependencies.SlowRequestThreshold, dependencies.BodyCaptureLimit, logger))

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", readinessHandler(dependencies.Repository, dependencies.HealthClient))

	userHandler := &UserHandler{client: dependencies.UserClient, timeout: dependencies.RPCTimeout}
	api := router.Group("/api/v1", dependencies.Rules.Middleware(), dependencies.Limiter.Middleware())
	api.GET("/users/:id", userHandler.GetUser)
	return router
}

func NewAdminHandler(token string, rules *IPRules, limiter *RateLimiter, logger *slog.Logger) *gin.Engine {
	router := newRouter(logger)
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	admin := router.Group("/admin/rules", adminAuthentication(token))
	admin.GET("/blacklist", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ips": rules.List()})
	})
	admin.PUT("/blacklist/:ip", func(c *gin.Context) {
		created, err := rules.Add(c.Param("ip"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_IP", err.Error())
			return
		}
		statusCode := http.StatusOK
		if created {
			statusCode = http.StatusCreated
		}
		c.JSON(statusCode, gin.H{"ip": c.Param("ip")})
	})
	admin.DELETE("/blacklist/:ip", func(c *gin.Context) {
		removed, err := rules.Remove(c.Param("ip"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_IP", err.Error())
			return
		}
		if !removed {
			abortWithError(c, http.StatusNotFound, "RULE_NOT_FOUND", "IP is not blocked")
			return
		}
		c.Status(http.StatusNoContent)
	})
	admin.GET("/rate-limit", func(c *gin.Context) {
		rps, burst := limiter.Settings()
		c.JSON(http.StatusOK, gin.H{"requests_per_second": rps, "burst": burst})
	})
	admin.PUT("/rate-limit", func(c *gin.Context) {
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
	router.HandleMethodNotAllowed = true
	router.Use(RequestID())
	if len(middleware) > 0 {
		router.Use(middleware...)
	}
	router.Use(Recovery(logger))
	router.NoRoute(func(c *gin.Context) {
		abortWithError(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "route not found")
	})
	router.NoMethod(func(c *gin.Context) {
		abortWithError(c, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})
	return router
}

func readinessHandler(repository Pinger, healthClient HealthChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		databaseReady := repository.Ping(ctx) == nil
		health, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{Service: userv1.UserService_ServiceDesc.ServiceName})
		userServiceReady := err == nil && health.GetStatus() == healthpb.HealthCheckResponse_SERVING
		if !databaseReady || !userServiceReady {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"checks": gin.H{"database": databaseReady, "user_service": userServiceReady},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
			"checks": gin.H{"database": databaseReady, "user_service": userServiceReady},
		})
	}
}
