package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

// TrafficLogReader is the read-only query contract used by gateway admin
// observability handlers. *trafficlog.Repository satisfies this interface.
type TrafficLogReader interface {
	OverviewStats(ctx context.Context, window time.Duration, route string) (trafficlog.OverviewStats, error)
	TimeSeries(ctx context.Context, window, step time.Duration, route string) ([]trafficlog.TimeSeriesPoint, error)
	RouteStats(ctx context.Context, window time.Duration, groupBy, sortBy string, limit int) ([]trafficlog.RouteStat, error)
	ListLogs(ctx context.Context, filter trafficlog.LogFilter) ([]trafficlog.LogItem, int64, error)
	GetLogByID(ctx context.Context, id int64) (*trafficlog.LogDetail, error)
	GetLogByRequestID(ctx context.Context, requestID string) (*trafficlog.LogDetail, error)
	GetLogsByTraceID(ctx context.Context, traceID string, limit int) ([]trafficlog.LogDetail, error)
	TopClients(ctx context.Context, window time.Duration, route string, limit int) ([]trafficlog.TopClientStat, error)
}

// SystemInfo describes gateway runtime capabilities and route configuration
// exposed by GET /admin/system.
type SystemInfo struct {
	AuthMode     string
	LoginEnabled bool
	OTELEnabled  bool
	TempoEnabled bool
	RedisUp      func() bool
}

type routeView struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
}

func parseWindowParam(raw string) (time.Duration, time.Duration, error) {
	switch strings.TrimSpace(raw) {
	case "", "1h":
		return time.Hour, time.Minute, nil
	case "15m":
		return 15 * time.Minute, 30 * time.Second, nil
	case "6h":
		return 6 * time.Hour, 5 * time.Minute, nil
	case "24h":
		return 24 * time.Hour, 15 * time.Minute, nil
	case "7d":
		return 7 * 24 * time.Hour, 2 * time.Hour, nil
	default:
		return 0, 0, errors.New("window must be one of 15m, 1h, 6h, 24h, 7d")
	}
}

func systemInfoHandler(routes *RouteTable, info SystemInfo, metrics *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := authz.PrincipalFromContext(c)
		principalPayload := gin.H{}
		if principal != nil {
			principalPayload = gin.H{
				"username":             principal.Subject,
				"user_id":              principal.UserID,
				"role":                 principal.Role,
				"type":                 principal.Type,
				"must_change_password": principal.PasswordChangeRequired,
			}
		}
		routeList := make([]routeView, 0)
		if routes != nil {
			for _, r := range routes.Routes() {
				routeList = append(routeList, routeView{
					Prefix:   r.Prefix,
					Upstream: r.Upstream.String(),
				})
			}
		}
		redisUp := true
		if info.RedisUp != nil {
			redisUp = info.RedisUp()
		} else if metrics != nil {
			redisUp = metrics.RedisUp()
		}
		authMode := info.AuthMode
		if authMode == "" {
			authMode = string(authz.ModeLegacy)
		}
		c.JSON(http.StatusOK, gin.H{
			"principal": principalPayload,
			"routes":    routeList,
			"capabilities": gin.H{
				"auth_mode":     authMode,
				"login_enabled": info.LoginEnabled,
				"otel_enabled":  info.OTELEnabled,
				"tempo_enabled": info.TempoEnabled,
			},
			"health": gin.H{
				"redis_up": redisUp,
			},
		})
	}
}

func observabilityOverviewHandler(logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		window, _, err := parseWindowParam(c.Query("window"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_WINDOW", err.Error())
			return
		}
		route := strings.TrimSpace(c.Query("route"))
		stats, err := logs.OverviewStats(c.Request.Context(), window, route)
		if err != nil {
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to query traffic overview")
			return
		}
		c.JSON(http.StatusOK, gin.H{"overview": stats, "window": c.DefaultQuery("window", "1h")})
	}
}

func observabilityTimeSeriesHandler(logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		window, step, err := parseWindowParam(c.Query("window"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_WINDOW", err.Error())
			return
		}
		route := strings.TrimSpace(c.Query("route"))
		points, err := logs.TimeSeries(c.Request.Context(), window, step, route)
		if err != nil {
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to query traffic time series")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"points":       points,
			"window":       c.DefaultQuery("window", "1h"),
			"step_seconds": int(step.Seconds()),
		})
	}
}

func observabilityRoutesHandler(logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		window, _, err := parseWindowParam(c.Query("window"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_WINDOW", err.Error())
			return
		}
		groupBy := c.DefaultQuery("group_by", "route")
		sortBy := c.DefaultQuery("sort_by", "requests")
		limit := 50
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a positive integer")
				return
			}
			limit = parsed
		}
		stats, err := logs.RouteStats(c.Request.Context(), window, groupBy, sortBy, limit)
		if err != nil {
			if strings.Contains(err.Error(), "invalid ") {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
				return
			}
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to query route statistics")
			return
		}
		c.JSON(http.StatusOK, gin.H{"routes": stats, "group_by": groupBy, "sort_by": sortBy})
	}
}

func observabilityLogsHandler(logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		window, _, err := parseWindowParam(c.Query("window"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_WINDOW", err.Error())
			return
		}
		filter := trafficlog.LogFilter{
			Window:    window,
			Route:     strings.TrimSpace(c.Query("route")),
			Path:      strings.TrimSpace(c.Query("path")),
			RequestID: strings.TrimSpace(c.Query("request_id")),
			TraceID:   strings.TrimSpace(c.Query("trace_id")),
			ClientIP:  strings.TrimSpace(c.Query("client_ip")),
			Limit:     50,
		}
		if raw := strings.TrimSpace(c.Query("only_errors")); raw != "" {
			val, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "only_errors must be a boolean")
				return
			}
			filter.OnlyErrors = val
		}
		if raw := strings.TrimSpace(c.Query("status_min")); raw != "" {
			val, parseErr := strconv.Atoi(raw)
			if parseErr != nil {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "status_min must be an integer")
				return
			}
			filter.StatusMin = val
		}
		if raw := strings.TrimSpace(c.Query("status_max")); raw != "" {
			val, parseErr := strconv.Atoi(raw)
			if parseErr != nil {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "status_max must be an integer")
				return
			}
			filter.StatusMax = val
		}
		if raw := strings.TrimSpace(c.Query("min_latency_ms")); raw != "" {
			val, parseErr := strconv.ParseFloat(raw, 64)
			if parseErr != nil || val < 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "min_latency_ms must be non-negative")
				return
			}
			filter.MinLatencyMS = val
		}
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			val, parseErr := strconv.Atoi(raw)
			if parseErr != nil || val <= 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a positive integer")
				return
			}
			filter.Limit = val
		}
		if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
			val, parseErr := strconv.Atoi(raw)
			if parseErr != nil || val < 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "offset must be non-negative")
				return
			}
			filter.Offset = val
		}

		items, total, err := logs.ListLogs(c.Request.Context(), filter)
		if err != nil {
			if strings.Contains(err.Error(), "must ") {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
				return
			}
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to query request logs")
			return
		}
		c.JSON(http.StatusOK, gin.H{"logs": items, "total": total, "limit": filter.Limit, "offset": filter.Offset})
	}
}

func observabilityLogDetailHandler(logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "log id must be a positive integer")
			return
		}
		detail, err := logs.GetLogByID(c.Request.Context(), id)
		if errors.Is(err, trafficlog.ErrNotFound) {
			abortWithError(c, http.StatusNotFound, "LOG_NOT_FOUND", "traffic log entry not found")
			return
		}
		if err != nil {
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to load traffic log detail")
			return
		}
		c.JSON(http.StatusOK, gin.H{"log": detail})
	}
}

func observabilityTopIPsHandler(logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		window, _, err := parseWindowParam(c.Query("window"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_WINDOW", err.Error())
			return
		}
		limit := 20
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a positive integer")
				return
			}
			limit = parsed
		}
		clients, err := logs.TopClients(c.Request.Context(), window, strings.TrimSpace(c.Query("route")), limit)
		if err != nil {
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to query top client IPs")
			return
		}
		c.JSON(http.StatusOK, gin.H{"clients": clients})
	}
}
