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
	TimeoutQueue  chan<- incident.TimeoutEvent
	SlowThreshold time.Duration
	CaptureLimit  int64
	Logger        *slog.Logger
	Metrics       *Metrics
}

func AccessLog(options AccessLogOptions) gin.HandlerFunc {
	var dropped atomic.Uint64
	var droppedTimeouts atomic.Uint64
	return func(c *gin.Context) {
		started := time.Now()
		requestBody := captureRequestBody(c, options.CaptureLimit)
		capture := &responseCapture{
			ResponseWriter: c.Writer,
			body:           limitedBuffer{remaining: options.CaptureLimit},
		}
		c.Writer = capture
		c.Next()

		latency := time.Since(started)
		status := c.Writer.Status()
		entry := trafficlog.Entry{Base: trafficlog.BaseLog{
			RequestID: requestIDFromContext(c),
			ClientIP:  c.ClientIP(),
			Method:    c.Request.Method,
			Path:      c.Request.URL.Path,
			Status:    status,
			LatencyMS: float64(latency) / float64(time.Millisecond),
			CreatedAt: started.UTC(),
		}}
		if status >= http.StatusBadRequest || latency >= options.SlowThreshold {
			entry.Detail = &trafficlog.ErrorBody{
				RequestBody:  requestBody,
				ResponseBody: capture.body.String(),
			}
		}

		select {
		case options.Queue <- entry:
		default:
			count := dropped.Add(1)
			if count == 1 || count%100 == 0 {
				options.Logger.Warn("traffic log queue full; dropping entry", "dropped_total", count)
			}
		}
		options.Metrics.Observe(c, status, latency)
		timedOutValue, _ := c.Get(upstreamTimeoutKey)
		timedOut, _ := timedOutValue.(bool)
		if timedOut && options.TimeoutQueue != nil {
			event := incident.TimeoutEvent{
				EventID:    newRequestID(),
				RequestID:  entry.Base.RequestID,
				OccurredAt: time.Now().UTC(),
				Method:     entry.Base.Method,
				Path:       entry.Base.Path,
				Route:      routeLabel(c),
				ClientIP:   entry.Base.ClientIP,
				StatusCode: status,
				LatencyMS:  entry.Base.LatencyMS,
			}
			select {
			case options.TimeoutQueue <- event:
			default:
				count := droppedTimeouts.Add(1)
				if count == 1 || count%100 == 0 {
					options.Logger.Warn("timeout incident queue full; dropping event", "dropped_total", count)
				}
			}
		}
		options.Logger.Info("http request",
			"request_id", entry.Base.RequestID,
			"client_ip", entry.Base.ClientIP,
			"method", entry.Base.Method,
			"path", entry.Base.Path,
			"status", entry.Base.Status,
			"latency_ms", entry.Base.LatencyMS,
			"upstream_timeout", timedOut == true,
		)
	}
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
