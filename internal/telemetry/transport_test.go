package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/terraform-plugin-log/tfsdklog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func memoryTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	previous := otel.GetTracerProvider()
	previousPropagation := otel.GetTextMapPropagator()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
		otel.SetTextMapPropagator(previousPropagation)
	})
	return exporter
}

func TestTransportPrivacyAndPropagation(t *testing.T) {
	exporter := memoryTracing(t)
	member, _ := baggage.NewMember("operation", "test")
	bag, _ := baggage.New(member)
	ctx := baggage.ContextWithBaggage(context.Background(), bag)
	ctx, parent := otel.Tracer("test").Start(ctx, "parent")
	defer parent.End()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://private-host/api/mdm/devices/private-id/send_custom_command/?token=private-query",
		strings.NewReader("private-body"))
	if err != nil {
		t.Fatal(err)
	}
	request.URL.User = url.UserPassword("private-user", "private-password")
	request.Header.Set("Authorization", "private-token")
	request.Header.Set("Cookie", "private-cookie")
	transport := Transport{Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		remote := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(r.Header))
		if baggage.FromContext(remote).Member("operation").Value() != "test" {
			t.Error("baggage not propagated")
		}
		if r.Header.Get("Traceparent") == "" || r.Header.Get("Authorization") != "private-token" {
			t.Error("trace propagation or original request headers lost")
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("private-response"))}, nil
	})}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Traceparent") != "" {
		t.Error("caller request mutated")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want one completed HTTP span", len(spans))
	}
	span := spans[0]
	if span.Name != "POST /api/mdm/devices/{id}/send_custom_command/" || span.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("unexpected span name or parent: %s", span.Name)
	}
	if span.Status.Code != codes.Error || len(span.Events) != 0 {
		t.Fatal("HTTP failure should set status without recording exception details")
	}
	encoded, _ := json.Marshal(span)
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("private input reached telemetry: %s", encoded)
	}
}

func TestTransportErrorsAreSanitized(t *testing.T) {
	for _, test := range []struct {
		name string
		body io.ReadCloser
		err  error
		kind string
	}{
		{name: "network", err: errors.New("https://private-url private-token"), kind: "transport_error"},
		{name: "body read", body: errorBody{}, kind: "response_read_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exporter := memoryTracing(t)
			transport := Transport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: http.StatusOK, Body: test.body}, nil
			})}
			request, _ := http.NewRequest(http.MethodGet, "https://example.invalid/api/inventory/machines/.private-serial/meta/", nil)
			response, err := transport.RoundTrip(request)
			if !errors.Is(err, test.err) {
				t.Fatal("original error not preserved for caller")
			}
			if response != nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			spans := exporter.GetSpans()
			if len(spans) != 1 || spans[0].Status.Description != test.kind {
				t.Fatalf("unexpected span error classification: %#v", spans)
			}
			encoded, _ := json.Marshal(spans)
			if strings.Contains(string(encoded), "private-") {
				t.Fatal("raw error or request identifier exported")
			}
		})
	}
}

type errorBody struct{}

func (errorBody) Read([]byte) (int, error) { return 0, errors.New("private-response-error") }
func (errorBody) Close() error             { return nil }

func TestUnknownRoutesAndMethodsAreBounded(t *testing.T) {
	exporter := memoryTracing(t)
	request, _ := http.NewRequest("private-method", "https://example.invalid/private-prefix/api/private-route/private-device?private-query", nil)
	transport := Transport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})}
	_, _ = transport.RoundTrip(request)
	span := exporter.GetSpans()[0]
	if span.Name != "_OTHER /api/{route}" {
		t.Fatalf("unexpected span name: %s", span.Name)
	}
	want := []attribute.KeyValue{attribute.String("http.request.method", "_OTHER"), attribute.String("http.route", "/api/{route}"), attribute.Int("http.response.status_code", 204)}
	if len(span.Attributes) != len(want) {
		t.Fatalf("unexpected attributes: %#v", span.Attributes)
	}
	for index, attribute := range want {
		if span.Attributes[index] != attribute {
			t.Fatalf("unexpected attribute: %#v", span.Attributes[index])
		}
	}
}

func TestCommandRoutesUseTemplates(t *testing.T) {
	for path, want := range map[string]string{
		"/api/mdm/devices/commands/private-uuid/":                      "/api/mdm/devices/commands/{id}/",
		"/api/mdm/devices/commands/":                                   "/api/mdm/devices/commands/",
		"/private-prefix/api/inventory/machines/.private-serial/meta/": "/api/inventory/machines/{serial_number}/meta/",
	} {
		if got := routeTemplate(path); got != want {
			t.Errorf("route template = %q, want %q", got, want)
		}
	}
}

func TestFinishCorrelatesLogs(t *testing.T) {
	memoryTracing(t)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	ctx := tfsdklog.ContextWithStandardLogging(context.Background(), t.Name())
	ctx = tfsdklog.NewRootProviderLogger(ctx, tfsdklog.WithLevel(hclog.Debug))
	ctx, span := otel.Tracer("test").Start(ctx, "request")
	fields := map[string]any{"http.route": "/api/mdm/devices/{id}/"}
	finish(ctx, span, fields)
	for _, expected := range []string{"Zentral API request completed", "trace_id", span.SpanContext().TraceID().String(), "span_id", span.SpanContext().SpanID().String(), "/api/mdm/devices/{id}/"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("log missing %q: %s", expected, output.String())
		}
	}
}
