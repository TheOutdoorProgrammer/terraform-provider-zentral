package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectortraces "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTLPRequestLogsCorrelateWithTracesWithoutPrivateData(t *testing.T) {
	cleanEnvironment(t)
	var mu sync.Mutex
	var logs []*logspb.LogRecord
	var spans []*tracepb.Span
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Authorization") != "private-export-token" {
			t.Error("exporter lost protocol or configured authentication")
		}
		if bytes.Contains(body, []byte("private-")) {
			t.Error("private input reached collector")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/logs":
			var payload collectorlogs.ExportLogsServiceRequest
			if err := proto.Unmarshal(body, &payload); err != nil {
				t.Error(err)
			}
			for _, resourceLogs := range payload.ResourceLogs {
				checkResource(t, resourceLogs.Resource.Attributes)
				for _, scope := range resourceLogs.ScopeLogs {
					logs = append(logs, scope.LogRecords...)
				}
			}
		case "/v1/traces":
			var payload collectortraces.ExportTraceServiceRequest
			if err := proto.Unmarshal(body, &payload); err != nil {
				t.Error(err)
			}
			for _, resourceSpans := range payload.ResourceSpans {
				checkResource(t, resourceSpans.Resource.Attributes)
				for _, scope := range resourceSpans.ScopeSpans {
					spans = append(spans, scope.Spans...)
				}
			}
		default:
			t.Errorf("unexpected export path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=private-export-token")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "private.key=private-value,service.instance.id=private-device")
	shutdown, err := Initialize(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		request, _ := http.NewRequest(http.MethodPost, "https://private-host/api/mdm/devices/private-id/send_custom_command/?private-query", strings.NewReader("private-body"))
		request.Header.Set("Authorization", "private-api-token")
		transport := Transport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private-response"))}, nil
		})}
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 2 || len(spans) != 2 {
		t.Fatalf("got %d logs and %d spans, want two of each", len(logs), len(spans))
	}
	seenInfo, seenError := false, false
	for _, record := range logs {
		if len(record.TraceId) != 16 || len(record.SpanId) != 8 {
			t.Fatal("log has no trace/span correlation")
		}
		matched := false
		for _, span := range spans {
			matched = matched || bytes.Equal(record.TraceId, span.TraceId) && bytes.Equal(record.SpanId, span.SpanId)
		}
		if !matched {
			t.Fatal("log does not match an exported trace")
		}
		attrs := make(map[string]*commonpb.AnyValue)
		for _, attr := range record.Attributes {
			attrs[attr.Key] = attr.Value
		}
		if attrs["trace_id"].GetStringValue() != hex.EncodeToString(record.TraceId) || attrs["span_id"].GetStringValue() != hex.EncodeToString(record.SpanId) {
			t.Fatal("log correlation attributes do not match OTLP IDs")
		}
		if attrs["http.route"].GetStringValue() != "/api/mdm/devices/{id}/send_custom_command/" {
			t.Fatal("missing route template")
		}
		seenInfo = seenInfo || record.SeverityNumber == logspb.SeverityNumber_SEVERITY_NUMBER_INFO && record.Body.GetStringValue() == "Zentral API request completed"
		seenError = seenError || record.SeverityNumber == logspb.SeverityNumber_SEVERITY_NUMBER_ERROR && record.Body.GetStringValue() == "Zentral API request failed"
	}
	if !seenInfo || !seenError {
		t.Fatal("success and error outcomes not both exported")
	}
}

func checkResource(t *testing.T, attrs []*commonpb.KeyValue) {
	t.Helper()
	if len(attrs) != 2 {
		t.Errorf("resource contains %d attributes, want two", len(attrs))
	}
	for _, attr := range attrs {
		switch attr.Key {
		case "service.name":
			if attr.Value.GetStringValue() != "terraform-provider-zentral" {
				t.Error("unexpected service name")
			}
		case "service.version":
			if attr.Value.GetStringValue() != "test" {
				t.Error("unexpected service version")
			}
		default:
			t.Error("unexpected resource attribute")
		}
	}
}

func TestLogOnlyEndpointDoesNotStartTraceExporter(t *testing.T) {
	cleanEnvironment(t)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", server.URL+"/v1/logs")
	shutdown, err := Initialize(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	emitRequestLog(context.Background(), map[string]any{"http.route": "/api/{route}", "http.request.method": "GET", "private-field": "private-value"})
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/v1/logs" {
		t.Fatalf("unexpected exports: %#v", paths)
	}
	if requestLogger.Load() != nil {
		t.Fatal("logger still enabled after shutdown")
	}
}
