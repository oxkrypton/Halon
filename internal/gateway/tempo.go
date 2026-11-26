package gateway

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oxkrypton/Halon/internal/trafficlog"
)

var (
	// ErrTraceNotFound indicates that neither Tempo nor PostgreSQL holds the trace.
	ErrTraceNotFound = errors.New("trace not found")
	// ErrTempoUnavailable indicates an error querying the configured Tempo server.
	ErrTempoUnavailable = errors.New("tempo is unavailable")
)

// TraceSummary is one item returned by GET /admin/traces.
type TraceSummary struct {
	TraceID       string    `json:"trace_id"`
	RequestID     string    `json:"request_id,omitempty"`
	RootService   string    `json:"root_service"`
	RootOperation string    `json:"root_operation"`
	Route         string    `json:"route,omitempty"`
	StartTime     time.Time `json:"start_time"`
	DurationMS    float64   `json:"duration_ms"`
	SpanCount     int       `json:"span_count"`
	StatusCode    int       `json:"status_code,omitempty"`
	HasError      bool      `json:"has_error"`
	Source        string    `json:"source"`
}

// TraceEvent represents an OTLP span event.
type TraceEvent struct {
	Name       string         `json:"name"`
	TimeUnixNS int64          `json:"time_unix_ns"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// TraceSpan is a normalized span rendered by the frontend waterfall.
type TraceSpan struct {
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id,omitempty"`
	ServiceName   string         `json:"service_name"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind,omitempty"`
	StartUnixNS   int64          `json:"start_unix_ns"`
	EndUnixNS     int64          `json:"end_unix_ns"`
	OffsetMS      float64        `json:"offset_ms"`
	DurationMS    float64        `json:"duration_ms"`
	Status        string         `json:"status"`
	StatusMessage string         `json:"status_message,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Events        []TraceEvent   `json:"events,omitempty"`
}

// TraceDetail is the normalized trace payload returned by GET /admin/traces/:trace_id.
type TraceDetail struct {
	TraceID       string                 `json:"trace_id"`
	Source        string                 `json:"source"`
	RootService   string                 `json:"root_service"`
	RootOperation string                 `json:"root_operation"`
	StartTime     time.Time              `json:"start_time"`
	DurationMS    float64                `json:"duration_ms"`
	HasError      bool                   `json:"has_error"`
	Services      []string               `json:"services"`
	Spans         []TraceSpan            `json:"spans"`
	Logs          []trafficlog.LogDetail `json:"logs,omitempty"`
}

// TempoSearchParams filters trace searches against Tempo.
type TempoSearchParams struct {
	Start         time.Time
	End           time.Time
	Service       string
	Route         string
	MinDurationMS float64
	OnlyErrors    bool
	Limit         int
}

// TempoClient queries Grafana Tempo's HTTP API for trace search and detail.
type TempoClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewTempoClient constructs a TempoClient when baseURL is non-empty.
func NewTempoClient(baseURL string, timeout time.Duration) *TempoClient {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &TempoClient{
		baseURL:    trimmed,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Search queries Tempo's /api/search endpoint and normalizes results.
func (c *TempoClient) Search(ctx context.Context, params TempoSearchParams) ([]TraceSummary, error) {
	if c == nil {
		return nil, ErrTempoUnavailable
	}
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if !params.Start.IsZero() {
		q.Set("start", strconv.FormatInt(params.Start.Unix(), 10))
	}
	if !params.End.IsZero() {
		q.Set("end", strconv.FormatInt(params.End.Unix(), 10))
	}
	if params.MinDurationMS > 0 {
		q.Set("minDuration", fmt.Sprintf("%dms", int64(params.MinDurationMS)))
	}
	if traceQL := buildTraceQL(params); traceQL != "" {
		q.Set("q", traceQL)
	}

	endpoint := c.baseURL + "/api/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTempoUnavailable, err)
	}
	// #nosec G704 -- baseURL is validated at startup by LoadConfig as an http(s) URL.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTempoUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrTempoUnavailable, resp.StatusCode)
	}

	var payload struct {
		Traces []struct {
			TraceID           string `json:"traceID"`
			RootServiceName   string `json:"rootServiceName"`
			RootTraceName     string `json:"rootTraceName"`
			StartTimeUnixNano string `json:"startTimeUnixNano"`
			DurationMs        int64  `json:"durationMs"`
			SpanSets          []struct {
				Matched int `json:"matched"`
			} `json:"spanSets"`
			SpanSet *struct {
				Matched int `json:"matched"`
			} `json:"spanSet"`
		} `json:"traces"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: decode search response: %v", ErrTempoUnavailable, err)
	}

	results := make([]TraceSummary, 0, len(payload.Traces))
	for _, item := range payload.Traces {
		startNS, _ := strconv.ParseInt(item.StartTimeUnixNano, 10, 64)
		spanCount := 0
		if item.SpanSet != nil {
			spanCount = item.SpanSet.Matched
		}
		for _, ss := range item.SpanSets {
			spanCount += ss.Matched
		}
		results = append(results, TraceSummary{
			TraceID:       normalizeTraceHexID(item.TraceID),
			RootService:   item.RootServiceName,
			RootOperation: item.RootTraceName,
			StartTime:     time.Unix(0, startNS).UTC(),
			DurationMS:    float64(item.DurationMs),
			SpanCount:     spanCount,
			HasError:      params.OnlyErrors,
			Source:        "tempo",
		})
	}
	return results, nil
}

func buildTraceQL(params TempoSearchParams) string {
	var clauses []string
	if svc := sanitizeTraceQLValue(params.Service); svc != "" {
		clauses = append(clauses, fmt.Sprintf(`resource.service.name="%s"`, svc))
	}
	if route := sanitizeTraceQLValue(params.Route); route != "" {
		clauses = append(clauses, fmt.Sprintf(`span.url.path=~"^%s.*"`, route))
	}
	if params.OnlyErrors {
		clauses = append(clauses, `status=error`)
	}
	if len(clauses) == 0 {
		return ""
	}
	return "{" + strings.Join(clauses, " && ") + "}"
}

func sanitizeTraceQLValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 128 {
		return ""
	}
	var b strings.Builder
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '/' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// GetTrace fetches and normalizes one trace from Tempo's /api/traces/:id endpoint.
func (c *TempoClient) GetTrace(ctx context.Context, traceID string) (*TraceDetail, error) {
	if c == nil {
		return nil, ErrTempoUnavailable
	}
	if !isValidHexTraceID(traceID) {
		return nil, ErrTraceNotFound
	}
	endpoint := c.baseURL + "/api/traces/" + traceID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTempoUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	// #nosec G704 -- baseURL is validated at startup and traceID is strictly hex.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTempoUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrTraceNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrTempoUnavailable, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read trace: %v", ErrTempoUnavailable, err)
	}
	detail, err := decodeOTLPTrace(traceID, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTempoUnavailable, err)
	}
	return detail, nil
}

func isValidHexTraceID(id string) bool {
	if len(id) != 16 && len(id) != 32 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

type otlpAnyValue struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
}

type otlpAttribute struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

func (v otlpAnyValue) toAny() any {
	if v.StringValue != nil {
		return *v.StringValue
	}
	if v.DoubleValue != nil {
		return *v.DoubleValue
	}
	if v.BoolValue != nil {
		return *v.BoolValue
	}
	if len(v.IntValue) > 0 {
		var str string
		if err := json.Unmarshal(v.IntValue, &str); err == nil {
			if n, convErr := strconv.ParseInt(str, 10, 64); convErr == nil {
				return n
			}
			return str
		}
		var n int64
		if err := json.Unmarshal(v.IntValue, &n); err == nil {
			return n
		}
	}
	return nil
}

func attrsToMap(attrs []otlpAttribute) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		if val := attr.Value.toAny(); val != nil {
			out[attr.Key] = val
		}
	}
	return out
}

func decodeOTLPTrace(traceID string, raw []byte) (*TraceDetail, error) {
	var envelope struct {
		Batches []struct {
			Resource struct {
				Attributes []otlpAttribute `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					TraceID           string          `json:"traceId"`
					SpanID            string          `json:"spanId"`
					ParentSpanID      string          `json:"parentSpanId"`
					Name              string          `json:"name"`
					Kind              any             `json:"kind"`
					StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
					EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
					Attributes        []otlpAttribute `json:"attributes"`
					Events            []struct {
						Name         string          `json:"name"`
						TimeUnixNano json.RawMessage `json:"timeUnixNano"`
						Attributes   []otlpAttribute `json:"attributes"`
					} `json:"events"`
					Status struct {
						Code    any    `json:"code"`
						Message string `json:"message"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode OTLP JSON: %w", err)
	}

	var spans []TraceSpan
	serviceSet := map[string]struct{}{}
	var minStart, maxEnd int64
	hasError := false

	for _, batch := range envelope.Batches {
		resAttrs := attrsToMap(batch.Resource.Attributes)
		serviceName := "unknown"
		if s, ok := resAttrs["service.name"].(string); ok && s != "" {
			serviceName = s
		}
		serviceSet[serviceName] = struct{}{}

		for _, scope := range batch.ScopeSpans {
			for _, sp := range scope.Spans {
				startNS := parseNano(sp.StartTimeUnixNano)
				endNS := parseNano(sp.EndTimeUnixNano)
				if endNS < startNS {
					endNS = startNS
				}
				if minStart == 0 || (startNS > 0 && startNS < minStart) {
					minStart = startNS
				}
				if endNS > maxEnd {
					maxEnd = endNS
				}
				status := normalizeSpanStatus(sp.Status.Code)
				if status == "error" {
					hasError = true
				}
				events := make([]TraceEvent, 0, len(sp.Events))
				for _, ev := range sp.Events {
					events = append(events, TraceEvent{
						Name:       ev.Name,
						TimeUnixNS: parseNano(ev.TimeUnixNano),
						Attributes: attrsToMap(ev.Attributes),
					})
				}
				spans = append(spans, TraceSpan{
					SpanID:        normalizeOTLPID(sp.SpanID),
					ParentSpanID:  normalizeOTLPID(sp.ParentSpanID),
					ServiceName:   serviceName,
					Name:          sp.Name,
					Kind:          normalizeSpanKind(sp.Kind),
					StartUnixNS:   startNS,
					EndUnixNS:     endNS,
					DurationMS:    float64(endNS-startNS) / float64(time.Millisecond),
					Status:        status,
					StatusMessage: sp.Status.Message,
					Attributes:    attrsToMap(sp.Attributes),
					Events:        events,
				})
			}
		}
	}

	if len(spans) == 0 {
		return nil, ErrTraceNotFound
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].StartUnixNS == spans[j].StartUnixNS {
			return spans[i].DurationMS > spans[j].DurationMS
		}
		return spans[i].StartUnixNS < spans[j].StartUnixNS
	})

	rootService := spans[0].ServiceName
	rootOp := spans[0].Name
	for i := range spans {
		spans[i].OffsetMS = float64(spans[i].StartUnixNS-minStart) / float64(time.Millisecond)
		if spans[i].ParentSpanID == "" {
			rootService = spans[i].ServiceName
			rootOp = spans[i].Name
		}
	}

	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}
	sort.Strings(services)

	return &TraceDetail{
		TraceID:       strings.ToLower(traceID),
		Source:        "tempo",
		RootService:   rootService,
		RootOperation: rootOp,
		StartTime:     time.Unix(0, minStart).UTC(),
		DurationMS:    float64(maxEnd-minStart) / float64(time.Millisecond),
		HasError:      hasError,
		Services:      services,
		Spans:         spans,
	}, nil
}

func parseNano(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		v, _ := strconv.ParseInt(str, 10, 64)
		return v
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	return 0
}

func normalizeTraceHexID(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func normalizeOTLPID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if isHexString(trimmed) {
		return strings.ToLower(trimmed)
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) > 0 {
		return hex.EncodeToString(decoded)
	}
	return trimmed
}

func isHexString(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func normalizeSpanStatus(code any) string {
	switch v := code.(type) {
	case string:
		lower := strings.ToLower(v)
		switch {
		case strings.Contains(lower, "error") || lower == "2":
			return "error"
		case strings.Contains(lower, "ok") || lower == "1":
			return "ok"
		}
	case float64:
		switch int(v) {
		case 2:
			return "error"
		case 1:
			return "ok"
		}
	}
	return "unset"
}

func normalizeSpanKind(kind any) string {
	switch v := kind.(type) {
	case string:
		return strings.TrimPrefix(v, "SPAN_KIND_")
	case float64:
		switch int(v) {
		case 1:
			return "INTERNAL"
		case 2:
			return "SERVER"
		case 3:
			return "CLIENT"
		case 4:
			return "PRODUCER"
		case 5:
			return "CONSUMER"
		}
	}
	return "UNSPECIFIED"
}

// synthesizeTraceFromLogs builds a gateway + upstream waterfall when Tempo is disabled or missing the trace.
func synthesizeTraceFromLogs(traceKey string, logs []trafficlog.LogDetail) *TraceDetail {
	if len(logs) == 0 {
		return nil
	}
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].CreatedAt.Before(logs[j].CreatedAt)
	})
	first := logs[0]
	traceStartNS := first.CreatedAt.UnixNano()
	var maxEndNS int64
	hasError := false
	spans := make([]TraceSpan, 0, len(logs)*2)
	services := []string{"halon-gateway"}
	hasUpstream := false

	for idx, entry := range logs {
		if entry.StatusCode >= 500 || entry.UpstreamResult == UpstreamResultTimeout || entry.UpstreamResult == UpstreamResultError {
			hasError = true
		}
		startNS := entry.CreatedAt.UnixNano()
		totalNS := int64(entry.LatencyMS * float64(time.Millisecond))
		if totalNS < 0 {
			totalNS = 0
		}
		endNS := startNS + totalNS
		if endNS > maxEndNS {
			maxEndNS = endNS
		}

		rootSpanID := fmt.Sprintf("%016x", uint64(idx*2+1))
		gatewayStatus := "ok"
		if entry.StatusCode >= 500 {
			gatewayStatus = "error"
		}
		gatewayAttrs := map[string]any{
			"http.method":      entry.Method,
			"http.target":      entry.Path,
			"http.status_code": entry.StatusCode,
			"halon.route":      entry.Route,
			"halon.request_id": entry.RequestID,
			"client.address":   entry.ClientIP,
		}
		if entry.TraceID != "" {
			gatewayAttrs["otel.trace_id"] = entry.TraceID
		}
		spans = append(spans, TraceSpan{
			SpanID:      rootSpanID,
			ServiceName: "halon-gateway",
			Name:        entry.Method + " " + entry.Path,
			Kind:        "SERVER",
			StartUnixNS: startNS,
			EndUnixNS:   endNS,
			OffsetMS:    float64(startNS-traceStartNS) / float64(time.Millisecond),
			DurationMS:  entry.LatencyMS,
			Status:      gatewayStatus,
			Attributes:  gatewayAttrs,
		})

		if entry.UpstreamLatencyMS > 0 || entry.UpstreamResult != "" {
			hasUpstream = true
			upNS := int64(entry.UpstreamLatencyMS * float64(time.Millisecond))
			if upNS > totalNS {
				upNS = totalNS
			}
			// Center or place upstream inside gateway window (small gateway ingress overhead before upstream).
			overheadNS := (totalNS - upNS) / 2
			upStartNS := startNS + overheadNS
			upEndNS := upStartNS + upNS
			upStatus := "ok"
			if entry.UpstreamResult == UpstreamResultTimeout || entry.UpstreamResult == UpstreamResultError || entry.StatusCode >= 500 {
				upStatus = "error"
			}
			upAttrs := map[string]any{
				"halon.route":           entry.Route,
				"halon.upstream_result": entry.UpstreamResult,
				"halon.retries":         entry.Retries,
				"http.status_code":      entry.StatusCode,
			}
			opName := "upstream proxy"
			if entry.Route != "" {
				opName = "upstream " + entry.Route
			}
			spans = append(spans, TraceSpan{
				SpanID:       fmt.Sprintf("%016x", uint64(idx*2+2)),
				ParentSpanID: rootSpanID,
				ServiceName:  "upstream",
				Name:         opName,
				Kind:         "CLIENT",
				StartUnixNS:  upStartNS,
				EndUnixNS:    upEndNS,
				OffsetMS:     float64(upStartNS-traceStartNS) / float64(time.Millisecond),
				DurationMS:   entry.UpstreamLatencyMS,
				Status:       upStatus,
				Attributes:   upAttrs,
			})
		}
	}
	if hasUpstream {
		services = append(services, "upstream")
	}

	resolvedID := traceKey
	if first.TraceID != "" {
		resolvedID = first.TraceID
	} else if resolvedID == "" {
		resolvedID = first.RequestID
	}

	return &TraceDetail{
		TraceID:       resolvedID,
		Source:        "postgres",
		RootService:   "halon-gateway",
		RootOperation: first.Method + " " + first.Path,
		StartTime:     first.CreatedAt,
		DurationMS:    float64(maxEndNS-traceStartNS) / float64(time.Millisecond),
		HasError:      hasError,
		Services:      services,
		Spans:         spans,
		Logs:          logs,
	}
}
