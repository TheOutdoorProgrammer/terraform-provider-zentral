package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

type logHolder struct{ logger otellog.Logger }

var requestLogger atomic.Pointer[logHolder]

func initializeLogs(ctx context.Context, version string, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := otlploghttp.New(ctx,
		otlploghttp.WithTimeout(5*time.Second),
		otlploghttp.WithCompression(otlploghttp.NoCompression),
		otlploghttp.WithHTTPClient(&http.Client{Timeout: 5 * time.Second, Transport: logResourceTransport{base: http.DefaultTransport, version: version}}),
	)
	if err != nil {
		return nil, errors.New("could not initialize log exporter")
	}
	provider := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
	holder := &logHolder{logger: provider.Logger("terraform-provider-zentral/http")}
	requestLogger.Store(holder)
	return func(ctx context.Context) error {
		requestLogger.CompareAndSwap(holder, nil)
		return provider.Shutdown(ctx)
	}, nil
}

func emitRequestLog(ctx context.Context, fields map[string]any) {
	holder := requestLogger.Load()
	if holder == nil {
		return
	}
	var record otellog.Record
	record.SetTimestamp(time.Now())
	record.SetSeverity(otellog.SeverityInfo)
	record.SetSeverityText("INFO")
	record.SetBody(attribute.StringValue("Zentral API request completed"))
	if _, failed := fields["error.type"]; failed {
		record.SetSeverity(otellog.SeverityError)
		record.SetSeverityText("ERROR")
		record.SetBody(attribute.StringValue("Zentral API request failed"))
	}
	// This allowlist is independent of tflog so future diagnostic fields cannot
	// accidentally enter the exported telemetry stream.
	for _, key := range []string{"http.request.method", "http.route", "error.type", "trace_id", "span_id"} {
		if value, ok := fields[key].(string); ok {
			record.AddAttributes(attribute.String(key, value))
		}
	}
	if value, ok := fields["http.response.status_code"].(int); ok {
		record.AddAttributes(attribute.Int("http.response.status_code", value))
	}
	holder.logger.Emit(ctx, record)
}

// The log SDK merges environment resource attributes and exposes no record
// resource setter. Enforce the resource boundary on serialized OTLP requests,
// retaining the official exporter's batching, retries, headers, and shutdown.
type logResourceTransport struct {
	base    http.RoundTripper
	version string
}

func (t logResourceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	const maxExportSize = 8 << 20
	body, err := io.ReadAll(io.LimitReader(request.Body, maxExportSize+1))
	_ = request.Body.Close()
	if err != nil || len(body) > maxExportSize {
		return nil, errors.New("could not validate log export payload")
	}
	var payload collectorlogs.ExportLogsServiceRequest
	if proto.Unmarshal(body, &payload) != nil {
		return nil, errors.New("invalid log export payload")
	}
	for _, logs := range payload.ResourceLogs {
		logs.Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "terraform-provider-zentral"}}},
			{Key: "service.version", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: t.version}}},
		}}
		logs.SchemaUrl = ""
	}
	body, err = proto.Marshal(&payload)
	if err != nil {
		return nil, errors.New("could not encode log export payload")
	}
	clone := request.Clone(request.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.base.RoundTrip(clone)
}
