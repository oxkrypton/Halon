package gateway

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int64
}

func (b *limitedBuffer) Write(data []byte) {
	if b.remaining <= 0 {
		return
	}
	if int64(len(data)) > b.remaining {
		data = data[:b.remaining]
	}
	b.buffer.Write(data)
	b.remaining -= int64(len(data))
}

func (b *limitedBuffer) String() string {
	return strings.ToValidUTF8(b.buffer.String(), "\uFFFD")
}

type responseCapture struct {
	gin.ResponseWriter
	body limitedBuffer
}

func (w *responseCapture) Write(data []byte) (int, error) {
	w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *responseCapture) WriteString(data string) (int, error) {
	w.body.Write([]byte(data))
	return w.ResponseWriter.WriteString(data)
}

type AccessLogOptions struct {
	Queue         chan<- trafficlog.Entry
	Incidents     *IncidentSink
	SlowThreshold time.Duration
	CaptureLimit  int64
	Logger        *slog.Logger
	Metrics       *Metrics
}

func AccessLog(options AccessLogOptions) gin.HandlerFunc {
	var dropped atomic.Uint64
	return func(c *gin.Context) {
		started := time.Now()
		options.Metrics.IncInFlight()
		defer options.Metrics.DecInFlight()
		requestBody := captureRequestBody(c, options.CaptureLimit)
		capture := &responseCapture{
			ResponseWriter: c.Writer,
			body:           limitedBuffer{remaining: options.CaptureLimit},
		}
		c.Writer = capture
		c.Next()

		latency := time.Since(started)
		status := c.Writer.Status()
		route := routeLabel(c)
		traceID := traceIDFromContext(c.Request.Context())
		outcome, _ := c.Request.Context().Value(upstreamOutcomeKey{}).(*upstreamOutcome)
		entry := trafficlog.Entry{Base: trafficlog.BaseLog{
			RequestID: requestIDFromContext(c),
			TraceID:   traceID,
			ClientIP:  c.ClientIP(),
			Method:    c.Request.Method,
			Path:      c.Request.URL.Path,
			Route:     route,
			Status:    status,
			LatencyMS: float64(latency) / float64(time.Millisecond),
			CreatedAt: started.UTC(),
		}}
		if outcome != nil {
			entry.Base.UpstreamLatencyMS = float64(outcome.upstreamLatency) / float64(time.Millisecond)
			entry.Base.UpstreamResult = outcome.result
			entry.Base.Retries = outcome.retries
		}
		if status >= http.StatusBadRequest || latency >= options.SlowThreshold {
			entry.Detail = &trafficlog.ErrorBody{
				RequestBody:  requestBody,
				ResponseBody: capture.body.String(),
			}
		}

		select {
		case options.Queue <- entry:
		default:
			options.Metrics.IncTrafficlogDropped()
			count := dropped.Add(1)
			if count == 1 || count%100 == 0 {
				options.Logger.Warn("traffic log queue full; dropping entry", "dropped_total", count)
			}
		}
		options.Metrics.Observe(c, status, latency)
		incidentTypeValue, _ := c.Get(gatewayIncidentTypeKey)
		incidentType, _ := incidentTypeValue.(incident.EventType)
		timedOut := incidentType == incident.TypeUpstreamTimeout
		if options.Incidents != nil {
			if event, ok := buildGatewayIncident(incidentType, c.GetString(gatewayIncidentDetailKey), entry.Base, outcome); ok {
				options.Incidents.Emit(event)
			}
		}
		attributes := []any{
			"request_id", entry.Base.RequestID,
			"client_ip", entry.Base.ClientIP,
			"method", entry.Base.Method,
			"path", entry.Base.Path,
			"status", entry.Base.Status,
			"latency_ms", entry.Base.LatencyMS,
			"route", route,
			"upstream_timeout", timedOut,
		}
		if traceID != "" {
			attributes = append(attributes, "trace_id", traceID)
		}
		if outcome != nil {
			if outcome.result != "" {
				attributes = append(attributes, "upstream_result", outcome.result)
			}
			if outcome.retries > 0 {
				attributes = append(attributes, "retries", outcome.retries)
			}
		}
		options.Logger.Info("http request", attributes...)
	}
}

func buildGatewayIncident(classified incident.EventType, detail string, base trafficlog.BaseLog, outcome *upstreamOutcome) (incident.Event, bool) {
	eventType := classified
	if eventType == "" {
		if base.Status >= http.StatusInternalServerError && outcome != nil && outcome.result == UpstreamResultSuccess {
			eventType = incident.TypeUpstream5xx
		} else {
			return incident.Event{}, false
		}
	}
	scopeKey := base.Route
	if eventType == incident.TypeRateLimitAbuse {
		scopeKey = base.ClientIP
	}
	event := incident.Event{
		Type:       eventType,
		ScopeKey:   scopeKey,
		RequestID:  base.RequestID,
		OccurredAt: time.Now().UTC(),
		Method:     base.Method,
		Path:       base.Path,
		Route:      base.Route,
		ClientIP:   base.ClientIP,
		StatusCode: base.Status,
		LatencyMS:  base.LatencyMS,
	}
	if detail != "" {
		event.Details = map[string]any{"error": detail}
	}
	return event, true
}

func captureRequestBody(c *gin.Context, limit int64) string {
	if c.Request.Body == nil || c.Request.Body == http.NoBody {
		return ""
	}
	original := c.Request.Body
	data, err := io.ReadAll(io.LimitReader(original, limit))
	c.Request.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(data), original), Closer: original}
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}
