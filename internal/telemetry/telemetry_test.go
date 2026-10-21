package telemetry

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(key, "")
	}
	previous := otel.GetTracerProvider()
	previousPropagation := otel.GetTextMapPropagator()
	previousErrorHandler := otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		otel.SetTextMapPropagator(previousPropagation)
		otel.SetErrorHandler(previousErrorHandler)
	})
}

func TestInitializeWithoutEndpointHasNoExporter(t *testing.T) {
	cleanEnvironment(t)
	otel.SetTracerProvider(noop.NewTracerProvider())
	shutdown, err := Initialize(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "no-export")
	if span.IsRecording() {
		t.Fatal("tracing enabled without an explicit endpoint")
	}
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeExportsHTTPProtobufAndFlushes(t *testing.T) {
	for _, variable := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Run(variable, func(t *testing.T) {
			cleanEnvironment(t)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Content-Type") != "application/x-protobuf" || r.URL.Path != "/v1/traces" {
					t.Error("export did not use OTLP HTTP/protobuf")
				}
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte("terraform-provider-zentral")) || bytes.Contains(body, []byte("private-environment")) {
					t.Error("missing stable resource or leaked environment attributes")
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			endpoint := server.URL
			if variable == "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT" {
				endpoint += "/v1/traces"
			}
			t.Setenv(variable, endpoint)
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "private.attribute=private-environment")
			shutdown, err := Initialize(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			_, span := otel.Tracer("test").Start(context.Background(), "request")
			span.End()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatalf("got %d export requests, want one", requests.Load())
			}
		})
	}
}

func TestSDKDiagnosticsNeverIncludeSecrets(t *testing.T) {
	cleanEnvironment(t)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "private-invalid-header")
	shutdown, err := Initialize(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 || strings.Contains(output.String(), "private-invalid-header") {
		t.Fatal("SDK configuration diagnostic missing or leaked secret")
	}
}

func TestInvalidEndpointErrorIsSanitized(t *testing.T) {
	cleanEnvironment(t)
	endpoint := url.URL{Scheme: "https", Host: "example.invalid", Path: "/otlp", RawQuery: "private-query", User: url.UserPassword("private-user", "private-password")}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint.String())
	_, err := Initialize(context.Background(), "test")
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("invalid endpoint accepted or private endpoint echoed")
	}
}
