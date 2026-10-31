package httpapi

import (
	"context"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"hhc/asset-api/internal/telemetry"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerRootJoinsOnlyCanonicalDiagnosticRequestID(t *testing.T) {
	record := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(record), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() { otel.SetTracerProvider(old); _ = provider.Shutdown(context.Background()) }()
	handler := telemetry.Server(requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("A", 32), strings.Repeat("x", 128)} {
		r := httptest.NewRequest("GET", "/private?secret", nil)
		r.Header.Set("X-HHC-Request-ID", id)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Header().Get("X-HHC-Request-ID") != id {
			t.Fatal("diagnostic/audit request ID changed")
		}
	}
	spans := record.Ended()
	if len(spans) != 3 {
		t.Fatalf("spans=%d", len(spans))
	}
	for i, span := range spans {
		if span.Parent().IsValid() {
			t.Fatal("missing remote parent did not create a root")
		}
		got := ""
		for _, a := range span.Attributes() {
			if a.Key == "hhc.request_id" {
				got = a.Value.AsString()
			}
		}
		if i == 0 && got != strings.Repeat("a", 32) || i != 0 && got != "" {
			t.Fatalf("unexpected exported request ID %q", got)
		}
	}
}
