package telemetry

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"strings"
	"testing"
)

func TestWorkerRootAndFailurePrivacy(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	logs := captureDiagnosticLogs(t)
	old := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	defer func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(old) }()
	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	for _, kind := range []string{"scan", "derivative", "recording", "retention", "signature", "private-kind"} {
		ctx, finish := StartWorker(parentCtx, kind)
		if trace.SpanContextFromContext(ctx).TraceID() == parent.SpanContext().TraceID() {
			t.Fatal("worker must use independent bounded root")
		}
		finish(errors.New("secret-provider-payload"))
	}
	parent.End()
	spans := exporter.GetSpans()
	if len(spans) != 7 {
		t.Fatal("worker spans missing")
	}
	for _, span := range spans[:6] {
		if span.Status.Code != codes.Error || len(span.Events) != 0 || strings.Contains(span.Name, "private") {
			t.Fatal("unsafe worker trace")
		}
	}
	if strings.Contains(logs.String(), "secret-provider-payload") || !strings.Contains(logs.String(), `"phase":"worker"`) {
		t.Fatal("missing safe worker failure")
	}
}
