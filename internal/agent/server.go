package agent

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/incident"
)

const maxIncidentBodyBytes = 64 << 10

// ReportReader is the read contract the incident API needs.
type ReportReader interface {
	Get(ctx context.Context, eventID string) (*incident.Report, error)
	List(ctx context.Context, limit int) ([]incident.Report, error)
	Ping(ctx context.Context) error
}

type ServerDeps struct {
	Orchestrator *Orchestrator
	Deduper      *incident.Deduper
	Reports      ReportReader
	IngestToken  string
	Auth         *authz.Authenticator
	Logger       *slog.Logger
	Metrics      *Metrics
}

// NewHTTPHandler exposes the timeout incident ingest API and report queries.
func NewHTTPHandler(deps ServerDeps) *gin.Engine {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		panic(err)
	}
	router.Use(gin.Recovery())
	router.HandleMethodNotAllowed = true

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := deps.Reports.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": gin.H{"database": false}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "checks": gin.H{"database": true}})
	})

	if deps.Metrics != nil {
		router.GET("/metrics", gin.WrapH(deps.Metrics.Handler()))
	}

	authenticator := deps.Auth
	if authenticator == nil {
		// Legacy fallback used by existing callers: INGEST_TOKEN also grants
		// report reads, matching the pre-RBAC behaviour.
		authenticator, _ = authz.NewAuthenticator(
			authz.Config{Mode: authz.ModeLegacy, Issuer: authz.DefaultIssuer},
			authz.AudienceAgent, deps.IngestToken,
		)
	}

	api := router.Group("/api/v1")
	// The webhook keeps its single-purpose service credential in every mode.
	api.POST("/incidents/timeout", bearerAuth(deps.IngestToken), ingestTimeout(deps, logger))
	reports := api.Group("", authz.Middleware(authenticator, abortWithAPIError), authz.Require(authz.PermIncidentRead, abortWithAPIError))
	reports.GET("/incidents", listReports(deps.Reports))
	reports.GET("/incidents/:event_id", getReport(deps.Reports))

	router.NoRoute(func(c *gin.Context) {
		abortWithAPIError(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "route not found")
	})
	router.NoMethod(func(c *gin.Context) {
		abortWithAPIError(c, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})
	return router
}

func ingestTimeout(deps ServerDeps, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxIncidentBodyBytes)
		var event incident.TimeoutEvent
		if err := c.ShouldBindJSON(&event); err != nil {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT", "request body must be one valid timeout event JSON object")
			return
		}
		if err := event.Validate(); err != nil {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT", err.Error())
			return
		}
		if deps.Deduper != nil && deps.Deduper.Seen(event.EventID) {
			c.JSON(http.StatusOK, gin.H{"event_id": event.EventID, "accepted": false, "duplicate": true})
			return
		}
		duplicate, err := deps.Orchestrator.Submit(c.Request.Context(), event)
		if errors.Is(err, ErrQueueFull) {
			abortWithAPIError(c, http.StatusServiceUnavailable, "QUEUE_FULL", "too many incidents are waiting for investigation; retry later")
			return
		}
		if err != nil {
			logger.Error("submit incident", "event_id", event.EventID, "error", err)
			abortWithAPIError(c, http.StatusInternalServerError, "SUBMIT_FAILED", "failed to queue the incident")
			return
		}
		if duplicate {
			c.JSON(http.StatusOK, gin.H{"event_id": event.EventID, "accepted": false, "duplicate": true})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"event_id": event.EventID, "accepted": true, "duplicate": false})
	}
}

func listReports(reports ReportReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 50
		if raw := c.Query("limit"); raw != "" {
			var parsed int
			if _, err := fmt.Sscanf(raw, "%d", &parsed); err != nil {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_LIMIT", "limit must be an integer")
				return
			}
			limit = parsed
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		list, err := reports.List(ctx, limit)
		if err != nil {
			abortWithAPIError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to list incident reports")
			return
		}
		c.JSON(http.StatusOK, gin.H{"incidents": list})
	}
}

func getReport(reports ReportReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		eventID := strings.TrimSpace(c.Param("event_id"))
		if eventID == "" || len(eventID) > 64 {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT_ID", "event id is required")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		report, err := reports.Get(ctx, eventID)
		if errors.Is(err, incident.ErrNotFound) {
			abortWithAPIError(c, http.StatusNotFound, "REPORT_NOT_FOUND", "no report exists for this event id")
			return
		}
		if err != nil {
			abortWithAPIError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to load the incident report")
			return
		}
		c.JSON(http.StatusOK, gin.H{"incident": report})
	}
}

func bearerAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(token)) != 1 {
			abortWithAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "a valid bearer token is required")
			return
		}
		c.Next()
	}
}

type apiErrorResponse struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func abortWithAPIError(c *gin.Context, httpStatus int, code, message string) {
	c.AbortWithStatusJSON(httpStatus, apiErrorResponse{Error: apiErrorDetail{Code: code, Message: message}})
}
