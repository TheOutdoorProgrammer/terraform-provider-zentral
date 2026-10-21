package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Transport wraps the Zentral API transport. The zero value uses http.DefaultTransport.
// It records only allowlisted route templates, methods, status codes, and error classes.
type Transport struct {
	Base http.RoundTripper
}

func (t Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	method, route := safeMethod(request.Method), routeTemplate(request.URL.Path)
	ctx, span := otel.Tracer("terraform-provider-zentral/http").Start(request.Context(), method+" "+route,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", method), attribute.String("http.route", route)),
	)
	// RoundTripper must not mutate a request owned by its caller.
	request = request.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(request.Header))
	response, err := base.RoundTrip(request)
	fields := map[string]any{"http.request.method": method, "http.route": route}
	if response != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
		fields["http.response.status_code"] = response.StatusCode
	}
	if err != nil {
		markError(span, "transport_error")
		fields["error.type"] = "transport_error"
		finish(ctx, span, fields)
		return response, err
	}
	if response.StatusCode >= 400 {
		markError(span, "http_error")
		fields["error.type"] = "http_error"
	}
	if response.Body == nil || response.Body == http.NoBody {
		finish(ctx, span, fields)
	} else {
		response.Body = &responseBody{ReadCloser: response.Body, ctx: ctx, span: span, fields: fields}
	}
	return response, nil
}

func markError(span trace.Span, kind string) {
	span.SetStatus(codes.Error, kind)
	span.SetAttributes(attribute.String("error.type", kind))
}

func finish(ctx context.Context, span trace.Span, fields map[string]any) {
	if sc := span.SpanContext(); sc.IsValid() {
		fields["trace_id"] = sc.TraceID().String()
		fields["span_id"] = sc.SpanID().String()
	}
	if _, failed := fields["error.type"]; failed {
		tflog.Error(ctx, "Zentral API request failed", fields)
	} else {
		tflog.Debug(ctx, "Zentral API request completed", fields)
	}
	emitRequestLog(ctx, fields)
	span.End()
}

type responseBody struct {
	io.ReadCloser
	ctx    context.Context
	span   trace.Span
	fields map[string]any
	once   sync.Once
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		kind := ""
		if !errors.Is(err, io.EOF) {
			kind = "response_read_error"
		}
		b.end(kind)
	}
	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()
	kind := ""
	if err != nil {
		kind = "response_close_error"
	}
	b.end(kind)
	return err
}

func (b *responseBody) end(kind string) {
	b.once.Do(func() {
		if kind != "" {
			markError(b.span, kind)
			b.fields["error.type"] = kind
		}
		finish(b.ctx, b.span, b.fields)
	})
}

func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return method
	default:
		return "_OTHER"
	}
}

var routes = []struct {
	pattern  *regexp.Regexp
	template string
}{
	{regexp.MustCompile(`^inventory/machines/tags/$`), "/api/inventory/machines/tags/"},
	{regexp.MustCompile(`^inventory/machines/[^/]+/meta/$`), "/api/inventory/machines/{serial_number}/meta/"},
	{regexp.MustCompile(`^mdm/devices/[^/]+/send_custom_command/$`), "/api/mdm/devices/{id}/send_custom_command/"},
	{regexp.MustCompile(`^mdm/devices/commands/[^/]+/$`), "/api/mdm/devices/commands/{id}/"},
	{regexp.MustCompile(`^mdm/devices/commands/$`), "/api/mdm/devices/commands/"},
	{regexp.MustCompile(`^mdm/devices/[^/]+/commands/$`), "/api/mdm/devices/{id}/commands/"},
	{regexp.MustCompile(`^mdm/devices/[^/]+/$`), "/api/mdm/devices/{id}/"},
	{regexp.MustCompile(`^mdm/devices/$`), "/api/mdm/devices/"},
	{regexp.MustCompile(`^inventory/tags/$`), "/api/inventory/tags/"},
	{regexp.MustCompile(`^inventory/tags/[^/]+/$`), "/api/inventory/tags/{id}/"},
	{regexp.MustCompile(`^mdm/profiles/$`), "/api/mdm/profiles/"},
	{regexp.MustCompile(`^mdm/profiles/[^/]+/$`), "/api/mdm/profiles/{id}/"},
}

func routeTemplate(path string) string {
	// Installations can mount the API below a path prefix. Never retain that prefix.
	if index := strings.Index(path, "/api/"); index >= 0 {
		path = path[index+len("/api/"):]
		for _, route := range routes {
			if route.pattern.MatchString(path) {
				return route.template
			}
		}
	}
	return "/api/{route}"
}
