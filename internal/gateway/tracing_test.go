package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestTracingDisabledIsPassThrough(t *testing.T) {
	tracing, err := InitTracing(context.Background(), TracingConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if tracing != nil {
		t.Fatal("disabled tracing must return nil")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if got := tracing.WrapHandler(handler); got == nil {
		t.Fatal("disabled tracing must not nil the handler")
	} else if _, ok := got.(http.HandlerFunc); !ok {
		t.Fatal("disabled tracing must return the handler unwrapped")
	}
	if err := tracing.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTracingPropagatesTraceparentToUpstream(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	tracing := &Tracing{provider: provider}

	traceparent := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent <- r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler := tracing.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if traceIDFromContext(r.Context()) == "" {
			t.Error("server span must put a trace ID into the request context")
		}
		client := &http.Client{Transport: tracing.WrapTransport(http.DefaultTransport)}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
		if err != nil {
			t.Error(err)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_ = resp.Body.Close()
		w.WriteHeader(http.StatusOK)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := <-traceparent; got == "" {
		t.Fatal("upstream did not receive a traceparent header")
	}

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want server+client pair", len(spans))
	}
	if spans[0].SpanContext().TraceID() != spans[1].SpanContext().TraceID() {
		t.Fatalf("server and client spans must share one trace: %v vs %v",
			spans[0].SpanContext().TraceID(), spans[1].SpanContext().TraceID())
	}
}

func TestTraceIDFromContext(t *testing.T) {
	if got := traceIDFromContext(context.Background()); got != "" {
		t.Fatalf("traceIDFromContext(empty) = %q, want empty", got)
	}
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	if got := traceIDFromContext(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("traceIDFromContext = %q", got)
	}
}
