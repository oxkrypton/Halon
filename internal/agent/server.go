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
	List(ctx context.Context, eventType incident.EventType, limit int) ([]incident.Report, error)
	Ping(ctx context.Context) error
}

type ServerDeps struct {
	Orchestrator *Orchestrator
	Deduper      *incident.Deduper
	Reports      ReportReader
	Pods         PodLogReader
	IngestToken  string
	Auth         *authz.Authenticator
	Logger       *slog.Logger
	Metrics      *Metrics
}

// NewHTTPHandler exposes the incident ingest API, session management, and report queries.
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
	// Webhook ingest keeps its single-purpose service credential in every mode.
	ingestAuth := bearerAuth(deps.IngestToken)
	api.POST("/incidents", ingestAuth, ingestIncident(deps, logger, ""))
	api.POST("/incidents/timeout", ingestAuth, ingestIncident(deps, logger, incident.TypeUpstreamTimeout))

	reports := api.Group("", authz.Middleware(authenticator, abortWithAPIError), authz.Require(authz.PermIncidentRead, abortWithAPIError))
	reports.GET("/incidents", listReports(deps.Reports))
	reports.GET("/incidents/:event_id", getReport(deps.Reports))
	reports.GET("/sessions", listSessions(deps.Orchestrator))

	sessionManage := api.Group("", authz.Middleware(authenticator, abortWithAPIError), authz.Require(authz.PermIncidentManage, abortWithAPIError))
	sessionManage.POST("/sessions/:event_id/cancel", cancelSession(deps.Orchestrator))

	k8s := api.Group("/k8s", authz.Middleware(authenticator, abortWithAPIError), authz.Require(authz.PermPodLogsRead, abortWithAPIError))
	k8s.GET("/pods", listPods(deps.Pods))
	k8s.GET("/pods/:pod/logs", getPodLogs(deps.Pods))

	router.NoRoute(func(c *gin.Context) {
		abortWithAPIError(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "route not found")
	})
	router.NoMethod(func(c *gin.Context) {
		abortWithAPIError(c, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})
	return router
}

func ingestIncident(deps ServerDeps, logger *slog.Logger, defaultType incident.EventType) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxIncidentBodyBytes)
		var event incident.Event
		if err := c.ShouldBindJSON(&event); err != nil {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT", "request body must be one valid incident event JSON object")
			return
		}
		if event.Type == "" && defaultType != "" {
			event.Type = defaultType
		} else if event.Type != "" {
			parsed, err := incident.ParseEventType(string(event.Type))
			if err != nil {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT", err.Error())
				return
			}
			event.Type = parsed
		}
		if event.OccurredAt.IsZero() {
			event.OccurredAt = time.Now().UTC()
		}
		event.ScopeKey = event.EffectiveScopeKey()
		if err := event.Validate(); err != nil {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT", err.Error())
			return
		}
		if deps.Deduper != nil && deps.Deduper.Seen(event.EventID) {
			deps.Metrics.IncIncidentAdmission(event.Type, AdmissionDuplicate)
			c.JSON(http.StatusOK, gin.H{
				"event_id":   event.EventID,
				"event_type": event.Type,
				"accepted":   false,
				"duplicate":  true,
				"suppressed": false,
			})
			return
		}
		result, err := deps.Orchestrator.Submit(c.Request.Context(), event)
		if errors.Is(err, ErrQueueFull) {
			abortWithAPIError(c, http.StatusServiceUnavailable, "QUEUE_FULL", "too many incidents are waiting for investigation; retry later")
			return
		}
		if errors.Is(err, ErrOrchestratorClosed) {
			abortWithAPIError(c, http.StatusServiceUnavailable, "AGENT_SHUTTING_DOWN", "agent is shutting down")
			return
		}
		if err != nil {
			logger.Error("submit incident", "event_id", event.EventID, "event_type", event.Type, "error", err)
			abortWithAPIError(c, http.StatusInternalServerError, "SUBMIT_FAILED", "failed to queue the incident")
			return
		}
		switch result.Outcome {
		case SubmitDuplicate:
			deps.Metrics.IncIncidentAdmission(event.Type, AdmissionDuplicate)
			c.JSON(http.StatusOK, gin.H{
				"event_id":   event.EventID,
				"event_type": event.Type,
				"accepted":   false,
				"duplicate":  true,
				"suppressed": false,
			})
		case SubmitSuppressed:
			deps.Metrics.IncIncidentAdmission(event.Type, AdmissionSuppressed)
			c.JSON(http.StatusOK, gin.H{
				"event_id":    event.EventID,
				"event_type":  event.Type,
				"incident_id": result.IncidentID,
				"accepted":    false,
				"duplicate":   false,
				"suppressed":  true,
			})
		default:
			deps.Metrics.IncIncidentAdmission(event.Type, AdmissionAccepted)
			c.JSON(http.StatusAccepted, gin.H{
				"event_id":    event.EventID,
				"event_type":  event.Type,
				"incident_id": result.IncidentID,
				"accepted":    true,
				"duplicate":   false,
				"suppressed":  false,
			})
		}
	}
}

func listReports(reports ReportReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		var eventType incident.EventType
		if rawType := strings.TrimSpace(c.Query("type")); rawType != "" {
			parsed, err := incident.ParseEventType(rawType)
			if err != nil {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT_TYPE", err.Error())
				return
			}
			eventType = parsed
		}
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
		list, err := reports.List(ctx, eventType, limit)
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

func listSessions(orchestrator *Orchestrator) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessions := []SessionSnapshot{}
		if orchestrator != nil {
			sessions = orchestrator.ListSessions()
		}
		c.JSON(http.StatusOK, gin.H{"sessions": sessions})
	}
}

func cancelSession(orchestrator *Orchestrator) gin.HandlerFunc {
	return func(c *gin.Context) {
		eventID := strings.TrimSpace(c.Param("event_id"))
		if eventID == "" || len(eventID) > 64 {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_EVENT_ID", "event id is required")
			return
		}
		if orchestrator == nil {
			abortWithAPIError(c, http.StatusServiceUnavailable, "ORCHESTRATOR_UNAVAILABLE", "orchestrator is unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		err := orchestrator.CancelSession(ctx, eventID)
		if errors.Is(err, ErrSessionNotFound) {
			abortWithAPIError(c, http.StatusNotFound, "SESSION_NOT_FOUND", "no active session exists for this event id")
			return
		}
		if err != nil {
			abortWithAPIError(c, http.StatusInternalServerError, "CANCEL_FAILED", "failed to cancel investigation session")
			return
		}
		c.JSON(http.StatusOK, gin.H{"session_id": eventID, "status": "cancelled"})
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
