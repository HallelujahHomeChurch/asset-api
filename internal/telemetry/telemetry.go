// Package telemetry exports bounded transport diagnostics, never business data.
package telemetry

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func Configure(ctx context.Context) func(context.Context) error {
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" {
		return func(context.Context) error { return nil }
	}
	// Export failures can contain endpoint credentials; never log the original error.
	var exportWarning sync.Once
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { exportWarning.Do(func() { slog.Warn("trace export failed") }) }))
	initCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	exporter, err := otlptracegrpc.New(initCtx)
	if err != nil {
		slog.Warn("trace exporter unavailable")
		return func(context.Context) error { return nil }
	}
	serviceResource := resource.NewSchemaless(
		attribute.String("service.name", "asset-api"),
		attribute.String("service.version", envOr("RELEASE", envOr("CONTAINER_APP_REVISION", "unknown"))),
		attribute.String("deployment.environment.name", envOr("ENVIRONMENT", "production")),
	)
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(serviceResource),
		sdktrace.WithBatcher(resourceExporter{SpanExporter: exporter, resource: serviceResource}, sdktrace.WithMaxQueueSize(512), sdktrace.WithMaxExportBatchSize(128), sdktrace.WithExportTimeout(3*time.Second)),
		sdktrace.WithSampler(traceSampler()),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown
}

// shortcut: caller-selected trace IDs can force sampling; platform owner reviews quotas in the next cost phase.
func traceSampler() sdktrace.Sampler {
	root := sdktrace.TraceIDRatioBased(sampleRatio())
	return sdktrace.ParentBased(root, sdktrace.WithRemoteParentSampled(root))
}

func sampleRatio() float64 {
	value, err := strconv.ParseFloat(os.Getenv("OTEL_TRACES_SAMPLER_ARG"), 64)
	if err != nil || value < 0 || value > 0.1 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0.01
	}
	return value
}
func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Server never gives an HTTP instrumentor access to URLs, headers or peer identity.
func Server(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if headers := r.Header.Values("traceparent"); len(headers) == 1 && validParent(headers[0]) {
			ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": headers[0]})
		}
		ctx, span := otel.Tracer("asset-api/http").Start(ctx, "asset.http", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("http.request.method", method(r.Method))))
		defer span.End()
		metrics := httpsnoop.CaptureMetrics(next, w, r.WithContext(ctx))
		span.SetAttributes(attribute.Int("http.response.status_code", metrics.Code))
		if metrics.Code >= 500 {
			span.SetStatus(codes.Error, "")
		}
	})
}
func validParent(value string) bool {
	if len(value) != 55 || value[:3] != "00-" || value[35] != '-' || value[52] != '-' || value[53:] != "00" && value[53:] != "01" || strings.ToLower(value) != value {
		return false
	}
	_, tErr := trace.TraceIDFromHex(value[3:35])
	_, sErr := trace.SpanIDFromHex(value[36:52])
	return tErr == nil && sErr == nil
}
func method(value string) string {
	switch value {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return value
	}
	return "_OTHER"
}

// AuditTransport is used only for the fixed internal Audit endpoint, never storage.
type AuditTransport struct{ Base http.RoundTripper }

func (t AuditTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer("asset-api/http").Start(r.Context(), "audit.http", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("http.request.method", method(r.Method))))
	defer span.End()
	request := r.Clone(ctx)
	request.Header = r.Header.Clone()
	if trace.SpanContextFromContext(ctx).IsValid() {
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(request.Header))
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if err != nil {
		span.SetStatus(codes.Error, "")
		span.SetAttributes(attribute.String("error.type", "transport_error"))
	} else {
		span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
		if response.StatusCode >= 500 {
			span.SetStatus(codes.Error, "")
		}
	}
	return response, err
}

// WithResource also merges environment attributes; restrict the actual export boundary.
type resourceExporter struct {
	sdktrace.SpanExporter
	resource *resource.Resource
}

func (e resourceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	safe := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		safe[i] = resourceSpan{ReadOnlySpan: span, resource: e.resource}
	}
	return e.SpanExporter.ExportSpans(ctx, safe)
}

type resourceSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s resourceSpan) Resource() *resource.Resource { return s.resource }
