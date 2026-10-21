// Package telemetry provides opt-in tracing without recording configuration or device data.
package telemetry

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Initialize enables OTLP HTTP/protobuf tracing only when an endpoint is explicitly
// configured. Call once at process startup and shut down before exiting the plugin.
func Initialize(ctx context.Context, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	noop := func(context.Context) error { return nil }
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" && strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) == "" {
		return noop, nil
	}

	// SDK configuration and exporter failures can contain URLs, headers, or response
	// bodies. Replace their diagnostic sinks before the SDK parses environment values.
	otel.SetLogger(logr.New(safeSDKLog{}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { reportSDKFailure() }))
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		if value := os.Getenv(key); value != "" {
			u, err := url.Parse(value)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return noop, errors.New("invalid OpenTelemetry endpoint configuration")
			}
		}
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithTimeout(5*time.Second))
	if err != nil {
		return noop, errors.New("could not initialize OpenTelemetry exporter")
	}
	stableResource := resource.NewSchemaless(
		attribute.String("service.name", "terraform-provider-zentral"),
		attribute.String("service.version", version),
	)
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(resourceExporter{SpanExporter: exporter, resource: stableResource}),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(stableResource),
	)
	otel.SetTracerProvider(provider)
	return func(ctx context.Context) error {
		if err := provider.Shutdown(ctx); err != nil {
			return errors.New("OpenTelemetry shutdown failed")
		}
		return nil
	}, nil
}

// WithResource merges OTEL_RESOURCE_ATTRIBUTES even when given an explicit
// resource. Restrict the exported resource rather than mutating process-wide env.
type resourceExporter struct {
	sdktrace.SpanExporter
	resource *resource.Resource
}

func (e resourceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	safe := make([]sdktrace.ReadOnlySpan, len(spans))
	for index, span := range spans {
		safe[index] = resourceSpan{ReadOnlySpan: span, resource: e.resource}
	}
	return e.SpanExporter.ExportSpans(ctx, safe)
}

type resourceSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s resourceSpan) Resource() *resource.Resource { return s.resource }

func reportSDKFailure() {
	log.Print(`{"level":"error","message":"OpenTelemetry SDK or export failed; check endpoint, credentials, and connectivity"}`)
}

type safeSDKLog struct{}

func (safeSDKLog) Init(logr.RuntimeInfo)            {}
func (safeSDKLog) Enabled(int) bool                 { return false }
func (safeSDKLog) Info(int, string, ...any)         {}
func (safeSDKLog) Error(error, string, ...any)      { reportSDKFailure() }
func (s safeSDKLog) WithValues(...any) logr.LogSink { return s }
func (s safeSDKLog) WithName(string) logr.LogSink   { return s }
