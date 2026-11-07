package telemetry

import (
	"context"
	"errors"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"net"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	exportcollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type boundaryReceiver struct {
	exportcollector.UnimplementedTraceServiceServer
	fail    bool
	partial bool
}

func (r boundaryReceiver) Export(context.Context, *exportcollector.ExportTraceServiceRequest) (*exportcollector.ExportTraceServiceResponse, error) {
	if r.fail {
		return nil, grpcstatus.Error(codes.InvalidArgument, "private-collector-credential")
	}
	if r.partial {
		return &exportcollector.ExportTraceServiceResponse{PartialSuccess: &exportcollector.ExportTracePartialSuccess{RejectedSpans: 1, ErrorMessage: "private-collector-credential"}}, nil
	}
	return &exportcollector.ExportTraceServiceResponse{}, nil
}
func TestExportBoundaryEvidence(t *testing.T) {
	for _, mode := range []string{"accepted", "failed", "partial", "disabled", "initialization_failed"} {
		t.Run(mode, func(t *testing.T) {
			logs := captureDiagnosticLogs(t)
			old, handler := otel.GetTracerProvider(), otel.GetErrorHandler()
			defer otel.SetTracerProvider(old)
			defer otel.SetErrorHandler(handler)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			exportcollector.RegisterTraceServiceServer(server, boundaryReceiver{fail: mode == "failed", partial: mode == "partial"})
			go server.Serve(listener)
			defer server.Stop()
			endpoint := "http://" + listener.Addr().String()
			if mode == "disabled" {
				endpoint = ""
			}
			if mode == "initialization_failed" {
				endpoint = "http://private-endpoint:4317"
			}
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
			var stop func(context.Context) error
			if mode == "initialization_failed" {
				stop = configureExporter(context.Background(), func(context.Context) (sdktrace.SpanExporter, error) {
					return nil, errors.New("private-collector-credential")
				})
			} else {
				stop = Configure(context.Background())
			}
			if mode != "disabled" && mode != "initialization_failed" {
				tid, _ := trace.TraceIDFromHex("00000000000000000000000000000001")
				sid, _ := trace.SpanIDFromHex("1111111111111111")
				ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}))
				_, span := otel.Tracer("boundary-test").Start(ctx, "fixed-operation")
				span.End()
			}
			_ = stop(context.Background())
			got := logs.String()
			want := `"state":"initialized"`
			if mode == "disabled" {
				want = `"state":"disabled_missing_endpoint"`
			}
			if mode == "initialization_failed" {
				want = `"state":"initialization_failed"`
			}
			if !strings.Contains(got, want) {
				t.Fatalf("missing %s: %s", want, got)
			}
			if mode == "accepted" && !strings.Contains(got, `"accepted_spans_total":1`) {
				t.Fatal(got)
			}
			if (mode == "failed" || mode == "partial") && (!strings.Contains(got, `"failed_spans_total":1`) || strings.Contains(got, `"accepted_spans_total":1`)) {
				t.Fatal(got)
			}
			for _, secret := range []string{"private-collector-credential", "private-endpoint", listener.Addr().String()} {
				if strings.Contains(got, secret) {
					t.Fatalf("private exporter data leaked: %s", got)
				}
			}
		})
	}
}
