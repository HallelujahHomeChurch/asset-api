package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func captureDiagnosticLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &logs
}

func TestUnsampledFailureEvidenceAndPrivacy(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	old := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(old); _ = provider.Shutdown(context.Background()) })
	request := httptest.NewRequest("GET", "/private/user@example.test?token=secret", nil)
	request.Header.Set("Authorization", "secret")
	Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trace.SpanFromContext(r.Context()).IsRecording() {
			t.Fatal("unexpected sampled span")
		}
		w.WriteHeader(503)
	})).ServeHTTP(httptest.NewRecorder(), request)
	got := logs.String()
	for _, want := range []string{`"phase":"server"`, `"status":503`, `"sampled":false`, `"trace_id":`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s: %s", want, got)
		}
	}
	for _, forbidden := range []string{"secret", "user@example.test", "/private"} {
		if strings.Contains(got, forbidden) {
			t.Fatal("request data leaked")
		}
	}
}

type failingDiagnosticBody struct{}

func (failingDiagnosticBody) Read([]byte) (int, error) {
	return 0, errors.New("secret provider payload")
}
func (failingDiagnosticBody) Close() error { return errors.New("secret close") }

func TestBodyFailureOnceAndSlowSuccess(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	b := &diagnosticBody{ReadCloser: failingDiagnosticBody{}, ctx: context.Background(), started: time.Now()}
	_, _ = b.Read(make([]byte, 1))
	_ = b.Close()
	if strings.Count(logs.String(), "operation diagnostic") != 1 || !strings.Contains(logs.String(), `"phase":"client_body"`) || strings.Contains(logs.String(), "secret") {
		t.Fatalf("unsafe body diagnostics: %s", logs.String())
	}
	logs.Reset()
	RecordOutcome(context.Background(), "server", 200, 3*time.Second, false)
	if !strings.Contains(logs.String(), `"outcome":"slow"`) {
		t.Fatal("slow success missing")
	}
	logs.Reset()
	b = &diagnosticBody{ReadCloser: io.NopCloser(strings.NewReader("")), ctx: context.Background(), started: time.Now()}
	_, _ = b.Read(make([]byte, 1))
	_ = b.Close()
	if logs.Len() != 0 {
		t.Fatal("fast EOF logged as failure")
	}
}

func TestExporterFailuresCountedAndWarningsBounded(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	var failures exportErrors
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	failures.record(now)
	failures.record(now.Add(time.Second))
	failures.record(now.Add(time.Minute))
	if failures.total != 3 || strings.Count(logs.String(), "trace export failed") != 2 || !strings.Contains(logs.String(), `"sdk_errors_total":3`) {
		t.Fatalf("wrong exporter warning budget: %s", logs.String())
	}
}

func TestFailedProbeKeepsEvidence(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	Server(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	if !strings.Contains(logs.String(), `"status":503`) {
		t.Fatal("failed probe evidence missing")
	}
}

type eofCloseFailure struct{}

func (eofCloseFailure) Read([]byte) (int, error) { return 0, io.EOF }
func (eofCloseFailure) Close() error             { return errors.New("secret close failure") }
func TestBodyCloseFailureAfterEOF(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	b := &diagnosticBody{ReadCloser: eofCloseFailure{}, ctx: context.Background(), started: time.Now()}
	_, _ = b.Read(make([]byte, 1))
	_ = b.Close()
	_ = b.Close()
	if strings.Count(logs.String(), "operation diagnostic") != 1 || !strings.Contains(logs.String(), `"outcome":"failed"`) || strings.Contains(logs.String(), "secret") {
		t.Fatalf("close failure evidence: %s", logs.String())
	}
}

type diagnosticTransportFunc func(*http.Request) (*http.Response, error)

func (f diagnosticTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSlowHeadersDoNotClassifyFastBodyAsSlow(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	base := diagnosticTransportFunc(func(*http.Request) (*http.Response, error) {
		time.Sleep(slowOperation + 20*time.Millisecond)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	request, _ := http.NewRequest(http.MethodGet, "http://internal.invalid", nil)
	response, err := AuditTransport{Base: base}.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if !strings.Contains(logs.String(), `"phase":"client_headers"`) || strings.Contains(logs.String(), `"phase":"client_body"`) {
		t.Fatalf("incorrect phase attribution: %s", logs.String())
	}
}
