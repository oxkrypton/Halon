package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

func listTracesHandler(tempo *TempoClient, logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		window, _, err := parseWindowParam(c.Query("window"))
		if err != nil {
			abortWithError(c, http.StatusBadRequest, "INVALID_WINDOW", err.Error())
			return
		}
		route := strings.TrimSpace(c.Query("route"))
		service := strings.TrimSpace(c.Query("service"))
		source := strings.TrimSpace(c.DefaultQuery("source", "auto"))
		if source != "auto" && source != "tempo" && source != "postgres" {
			abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "source must be auto, tempo, or postgres")
			return
		}

		var minDurationMS float64
		if raw := strings.TrimSpace(c.Query("min_duration_ms")); raw != "" {
			parsed, parseErr := strconv.ParseFloat(raw, 64)
			if parseErr != nil || parsed < 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "min_duration_ms must be non-negative")
				return
			}
			minDurationMS = parsed
		}

		var onlyErrors bool
		if raw := strings.TrimSpace(c.Query("only_errors")); raw != "" {
			parsed, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "only_errors must be a boolean")
				return
			}
			onlyErrors = parsed
		}

		limit := 50
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed <= 0 {
				abortWithError(c, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a positive integer")
				return
			}
			if parsed > 100 {
				parsed = 100
			}
			limit = parsed
		}

		if (source == "auto" || source == "tempo") && tempo != nil {
			now := time.Now().UTC()
			traces, tempoErr := tempo.Search(c.Request.Context(), TempoSearchParams{
				Start:         now.Add(-window),
				End:           now,
				Service:       service,
				Route:         route,
				MinDurationMS: minDurationMS,
				OnlyErrors:    onlyErrors,
				Limit:         limit,
			})
			if tempoErr == nil {
				c.JSON(http.StatusOK, gin.H{"source": "tempo", "traces": traces})
				return
			}
			if source == "tempo" {
				abortWithError(c, http.StatusBadGateway, "TEMPO_UNAVAILABLE", "failed to query Tempo traces")
				return
			}
		} else if source == "tempo" && tempo == nil {
			abortWithError(c, http.StatusServiceUnavailable, "TEMPO_NOT_CONFIGURED", "TEMPO_URL is not configured")
			return
		}

		if logs == nil {
			abortWithError(c, http.StatusServiceUnavailable, "OBSERVABILITY_UNAVAILABLE", "traffic log store is not configured")
			return
		}
		items, _, err := logs.ListLogs(c.Request.Context(), trafficlog.LogFilter{
			Window:       window,
			Route:        route,
			MinLatencyMS: minDurationMS,
			OnlyErrors:   onlyErrors,
			Limit:        limit,
		})
		if err != nil {
			abortWithError(c, http.StatusInternalServerError, "QUERY_FAILED", "failed to query fallback trace summaries")
			return
		}
		traces := make([]TraceSummary, 0, len(items))
		for _, item := range items {
			traceKey := item.TraceID
			if traceKey == "" {
				traceKey = item.RequestID
			}
			spanCount := 1
			if item.UpstreamLatencyMS > 0 || item.UpstreamResult != "" {
				spanCount = 2
			}
			rootSvc := "halon-gateway"
			if item.Route != "" {
				rootSvc = "halon-gateway (" + item.Route + ")"
			}
			traces = append(traces, TraceSummary{
				TraceID:       traceKey,
				RequestID:     item.RequestID,
				RootService:   rootSvc,
				RootOperation: item.Method + " " + item.Path,
				Route:         item.Route,
				StartTime:     item.CreatedAt,
				DurationMS:    item.LatencyMS,
				SpanCount:     spanCount,
				StatusCode:    item.StatusCode,
				HasError:      item.StatusCode >= 400,
				Source:        "postgres",
			})
		}
		c.JSON(http.StatusOK, gin.H{"source": "postgres", "traces": traces})
	}
}

func getTraceHandler(tempo *TempoClient, logs TrafficLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		traceKey := strings.TrimSpace(c.Param("trace_id"))
		if traceKey == "" || len(traceKey) > 64 {
			abortWithError(c, http.StatusBadRequest, "INVALID_TRACE_ID", "trace id is required")
			return
		}

		var associatedLogs []trafficlog.LogDetail
		if logs != nil {
			if isValidHexTraceID(traceKey) {
				if byTrace, err := logs.GetLogsByTraceID(c.Request.Context(), strings.ToLower(traceKey), 20); err == nil {
					associatedLogs = byTrace
				}
			}
			if len(associatedLogs) == 0 {
				if byReq, err := logs.GetLogByRequestID(c.Request.Context(), traceKey); err == nil && byReq != nil {
					associatedLogs = []trafficlog.LogDetail{*byReq}
				}
			}
		}

		if tempo != nil && isValidHexTraceID(traceKey) {
			detail, err := tempo.GetTrace(c.Request.Context(), strings.ToLower(traceKey))
			if err == nil && detail != nil {
				detail.Logs = associatedLogs
				c.JSON(http.StatusOK, gin.H{"source": "tempo", "trace": detail})
				return
			}
			if err != nil && !errors.Is(err, ErrTraceNotFound) && len(associatedLogs) == 0 {
				abortWithError(c, http.StatusBadGateway, "TEMPO_UNAVAILABLE", "failed to load trace from Tempo")
				return
			}
		}

		if len(associatedLogs) > 0 {
			detail := synthesizeTraceFromLogs(traceKey, associatedLogs)
			c.JSON(http.StatusOK, gin.H{"source": "postgres", "trace": detail})
			return
		}
		abortWithError(c, http.StatusNotFound, "TRACE_NOT_FOUND", "no trace found for this id")
	}
}
