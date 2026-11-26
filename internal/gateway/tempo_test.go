package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/trafficlog"
)

func TestTempoClientSearchAndGetTrace(t *testing.T) {
	t.Parallel()
	const traceID = "0123456789abcdef0123456789abcdef"
	rootSpanBytes := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	childSpanBytes := []byte{8, 7, 6, 5, 4, 3, 2, 1}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			if got := r.URL.Query().Get("minDuration"); got != "500ms" {
				t.Fatalf("minDuration = %q, want 500ms", got)
			}
			if got := r.URL.Query().Get("q"); got == "" {
				t.Fatal("expected TraceQL q parameter")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"traces": []map[string]any{
					{
						"traceID":           traceID,
						"rootServiceName":   "halon-gateway",
						"rootTraceName":     "GET /api/v1/users/1",
						"startTimeUnixNano": "1764000000000000000",
						"durationMs":        120,
						"spanSet":           map[string]any{"matched": 2},
					},
				},
			})
		case "/api/traces/" + traceID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"batches": []map[string]any{
					{
						"resource": map[string]any{
							"attributes": []map[string]any{
								{"key": "service.name", "value": map[string]any{"stringValue": "halon-gateway"}},
							},
						},
						"scopeSpans": []map[string]any{
							{
								"spans": []map[string]any{
									{
										"traceId":           traceID,
										"spanId":            base64.StdEncoding.EncodeToString(rootSpanBytes),
										"name":              "halon-gateway",
										"kind":              2,
										"startTimeUnixNano": "1764000000000000000",
										"endTimeUnixNano":   "1764000000120000000",
										"status":            map[string]any{"code": 1},
									},
								},
							},
						},
					},
					{
						"resource": map[string]any{
							"attributes": []map[string]any{
								{"key": "service.name", "value": map[string]any{"stringValue": "user-service"}},
							},
						},
						"scopeSpans": []map[string]any{
							{
								"spans": []map[string]any{
									{
										"traceId":           traceID,
										"spanId":            base64.StdEncoding.EncodeToString(childSpanBytes),
										"parentSpanId":      base64.StdEncoding.EncodeToString(rootSpanBytes),
										"name":              "HTTP GET",
										"kind":              "SPAN_KIND_SERVER",
										"startTimeUnixNano": "1764000000010000000",
										"endTimeUnixNano":   "1764000000110000000",
										"attributes": []map[string]any{
											{"key": "http.status_code", "value": map[string]any{"intValue": "500"}},
										},
										"status": map[string]any{"code": "STATUS_CODE_ERROR", "message": "db timeout"},
									},
								},
							},
						},
					},
				},
			})
		case "/api/traces/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := NewTempoClient(server.URL, 2*time.Second)
	summaries, err := client.Search(context.Background(), TempoSearchParams{
		Start:         time.Unix(1763999000, 0),
		End:           time.Unix(1764001000, 0),
		Service:       "halon-gateway",
		Route:         "/api/v1/users/",
		MinDurationMS: 500,
		OnlyErrors:    true,
		Limit:         20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].TraceID != traceID || summaries[0].SpanCount != 2 {
		t.Fatalf("unexpected summaries: %+v", summaries)
	}

	detail, err := client.GetTrace(context.Background(), traceID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Source != "tempo" || !detail.HasError || len(detail.Spans) != 2 || len(detail.Services) != 2 {
		t.Fatalf("unexpected trace detail: %+v", detail)
	}
	if detail.Spans[1].ParentSpanID != detail.Spans[0].SpanID || detail.Spans[1].OffsetMS != 10 {
		t.Fatalf("unexpected child span linkage/offset: %+v", detail.Spans)
	}

	if _, err := client.GetTrace(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrTraceNotFound) {
		t.Fatalf("expected ErrTraceNotFound, got %v", err)
	}
	if _, err := client.GetTrace(context.Background(), "../etc/passwd"); !errors.Is(err, ErrTraceNotFound) {
		t.Fatalf("expected ErrTraceNotFound on invalid trace id, got %v", err)
	}
}

func TestSynthesizeTraceFromLogs(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	detail := synthesizeTraceFromLogs("req-1", []trafficlog.LogDetail{
		{
			LogItem: trafficlog.LogItem{
				ID:                1,
				RequestID:         "req-1",
				ClientIP:          "10.0.0.2",
				Method:            "GET",
				Path:              "/api/v1/orders/9",
				Route:             "/api/v1/orders/",
				StatusCode:        504,
				LatencyMS:         3005,
				UpstreamLatencyMS: 3000,
				UpstreamResult:    UpstreamResultTimeout,
				Retries:           1,
				CreatedAt:         now,
				HasBody:           true,
			},
			ResponseBody: `{"error":"timeout"}`,
		},
	})
	if detail == nil || detail.Source != "postgres" || !detail.HasError || len(detail.Spans) != 2 {
		t.Fatalf("unexpected synthesized trace: %+v", detail)
	}
	if detail.Spans[1].ParentSpanID != detail.Spans[0].SpanID {
		t.Fatalf("expected upstream span parent to match gateway span: %+v", detail.Spans)
	}
}
