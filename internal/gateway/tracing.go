package gateway

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// TracingConfig carries the OpenTelemetry settings. When Enabled is false
// every wrapper is a pass-through and no SDK is initialized.
type TracingConfig struct {
	Enabled      bool
	Endpoint     string
	SamplerRatio float64
}

// Tracing owns the tracer provider lifecycle and the HTTP wrappers.
type Tracing struct {
	provider *sdktrace.TracerProvider
}

// InitTracing sets up the OTLP gRPC exporter and tracer provider. It returns
// (nil, nil) when tracing is disabled, so call sites can use the wrappers
// unconditionally.
func InitTracing(ctx context.Context, cfg TracingConfig) (*Tracing, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplerRatio))),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("halon-gateway"),
		)),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return &Tracing{provider: provider}, nil
}

// WrapHandler instruments the data-plane HTTP handler with a server span per
// request. Disabled tracing returns the handler unchanged.
func (t *Tracing) WrapHandler(handler http.Handler) http.Handler {
	if t == nil {
		return handler
	}
	return otelhttp.NewHandler(handler, "halon-gateway")
}

// WrapTransport instruments an upstream transport with a client span per
// round trip and injects the W3C traceparent header; upstreams that do not
// recognize the header safely ignore it. Disabled tracing returns the
// transport unchanged.
func (t *Tracing) WrapTransport(transport http.RoundTripper) http.RoundTripper {
	if t == nil {
		return transport
	}
	return otelhttp.NewTransport(transport)
}

// Shutdown flushes pending spans and stops the provider.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

// traceIDFromContext extracts the W3C trace ID of the span carried by ctx,
// or "" when tracing is disabled or the span context is invalid.
func traceIDFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.HasTraceID() {
		return ""
	}
	return spanContext.TraceID().String()
}
