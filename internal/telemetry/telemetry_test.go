package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

const parent = "00-00000000000000000000000000000001-2222222222222222-01"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	old := otel.GetTracerProvider()
	record := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(record), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(old); _ = provider.Shutdown(context.Background()) })
	return record
}
func TestServerClientParentagePrivacyAndStreaming(t *testing.T) {
	record := recorder(t)
	request := httptest.NewRequest("GET", "http://private.invalid/assets/private-id?ticket=secret", nil)
	request.Header.Set("traceparent", parent)
	request.Header.Set("Authorization", "Bearer secret")
	original := request.Header.Clone()
	var sentParent string
	handler := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("streaming interface lost")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("application auth changed")
		}
		outbound := httptest.NewRequest("POST", "http://127.0.0.1/private?token=secret", nil).WithContext(r.Context())
		_, err := (AuditTransport{Base: roundTripFunc(func(out *http.Request) (*http.Response, error) {
			sentParent = out.Header.Get("traceparent")
			return &http.Response{StatusCode: 404, Body: http.NoBody, Header: http.Header{}}, nil
		})}).RoundTrip(outbound)
		if err != nil {
			t.Fatal(err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	spans := record.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans=%d", len(spans))
	}
	client, server := spans[0], spans[1]
	if server.Parent().SpanID().String() != "2222222222222222" || client.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Fatal("parent-child chain broken")
	}
	extracted := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": sentParent})
	if trace.SpanContextFromContext(extracted).SpanID() != client.SpanContext().SpanID() {
		t.Fatal("client span not propagated")
	}
	if request.Header.Get("traceparent") != original.Get("traceparent") {
		t.Fatal("audit provenance header changed")
	}
	for _, span := range spans {
		raw, _ := json.Marshal(span.Attributes())
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "private") {
			t.Fatal("private data exported")
		}
		for _, a := range span.Attributes() {
			if a.Key != "http.request.method" && a.Key != "http.response.status_code" {
				t.Fatalf("unexpected attribute %s", a.Key)
			}
		}
	}
}
func TestInvalidDuplicateUnsampledAndProbes(t *testing.T) {
	record := recorder(t)
	handler := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	for _, headers := range [][]string{{parent, parent}, {strings.Replace(parent, "-01", "-00", 1)}, {"00-00000000000000000000000000000000-2222222222222222-01"}} {
		r := httptest.NewRequest("GET", "http://private/path", nil)
		r.Header["Traceparent"] = headers
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	for _, path := range []string{"/health", "/ready"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("traceparent", parent)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(record.Ended()) != 0 {
		t.Fatal("invalid parent, probe or unsampled request recorded")
	}
}
func TestClientFailureDoesNotExportError(t *testing.T) {
	record := recorder(t)
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": parent})
	sentinel := errors.New("secret signed URL")
	request := httptest.NewRequest("GET", "http://private?secret", nil).WithContext(ctx)
	_, err := (AuditTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, sentinel })}).RoundTrip(request)
	if err != sentinel {
		t.Fatal("transport error changed")
	}
	spans := record.Ended()
	if len(spans) != 1 || len(spans[0].Events()) != 0 || spans[0].Status().Description != "" {
		t.Fatal("error leaked")
	}
}
func TestSampleRatio(t *testing.T) {
	for _, value := range []string{"", "junk", "NaN", "+Inf", "-1", "1"} {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", value)
		if sampleRatio() != 0.01 {
			t.Fatalf("invalid sample %s", value)
		}
	}
	for _, value := range []string{"0", "0.01", "0.1"} {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", value)
		if sampleRatio() < 0 || sampleRatio() > 0.1 {
			t.Fatal("valid rate rejected")
		}
	}
}

type receiver struct {
	collector.UnimplementedTraceServiceServer
	received chan *collector.ExportTraceServiceRequest
}

func (r receiver) Export(_ context.Context, req *collector.ExportTraceServiceRequest) (*collector.ExportTraceServiceResponse, error) {
	r.received <- req
	return &collector.ExportTraceServiceResponse{}, nil
}
func TestConfigureExportsToOTLP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	received := make(chan *collector.ExportTraceServiceRequest, 1)
	collector.RegisterTraceServiceServer(server, receiver{received: received})
	go server.Serve(listener)
	defer server.Stop()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.1")
	t.Setenv("RELEASE", "")
	t.Setenv("CONTAINER_APP_REVISION", "test-revision")
	t.Setenv("ENVIRONMENT", "test")
	old, oldHandler := otel.GetTracerProvider(), otel.GetErrorHandler()
	defer otel.SetTracerProvider(old)
	defer otel.SetErrorHandler(oldHandler)
	shutdown := Configure(context.Background())
	request := httptest.NewRequest("GET", "/private?secret", nil)
	request.Header.Set("traceparent", parent)
	Server(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })).ServeHTTP(httptest.NewRecorder(), request)
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-received:
		raw, _ := json.Marshal(req)
		if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "asset-api") || !strings.Contains(string(raw), "test-revision") {
			t.Fatalf("invalid export: %s", raw)
		}
	default:
		t.Fatal("no OTLP export")
	}
}
func TestConfigureDisabledPreservesProvider(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	old := otel.GetTracerProvider()
	if err := Configure(context.Background())(context.Background()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != old {
		t.Fatal("disabled tracing changed provider")
	}
}

func TestRemoteParentCannotOverrideLocalSampling(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.01")
	low, _ := trace.TraceIDFromHex("00000000000000000000000000000001")
	high, _ := trace.TraceIDFromHex("ffffffffffffffffffffffffffffffff")
	spanID, _ := trace.SpanIDFromHex("2222222222222222")
	for _, tc := range []struct {
		name            string
		id              trace.TraceID
		remote, sampled bool
		want            sdktrace.SamplingDecision
	}{
		{"remote sampled selected", low, true, true, sdktrace.RecordAndSample},
		{"remote sampled rejected", high, true, true, sdktrace.Drop},
		{"remote unsampled", low, true, false, sdktrace.Drop},
		{"local sampled", high, false, true, sdktrace.RecordAndSample},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := trace.TraceFlags(0)
			if tc.sampled {
				flags = trace.FlagsSampled
			}
			parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tc.id, SpanID: spanID, TraceFlags: flags, Remote: tc.remote})
			ctx := trace.ContextWithSpanContext(context.Background(), parent)
			for range 100 {
				result := traceSampler().ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx, TraceID: tc.id})
				if result.Decision != tc.want {
					t.Fatalf("decision=%v want=%v", result.Decision, tc.want)
				}
			}
		})
	}
}
