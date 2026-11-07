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
	request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-00")
	Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trace.SpanFromContext(r.Context()).IsRecording() {
			t.Fatal("unexpected sampled span")
		}
		w.WriteHeader(503)
	})).ServeHTTP(httptest.NewRecorder(), request)
	got := logs.String()
	for _, want := range []string{`"phase":"server"`, `"status":503`, `"sampled":false`, `"trace_id":`, `"parent_span_id":"0123456789abcdef"`} {
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

func TestDatabaseThresholdAndPrivacy(t *testing.T) {
	logs := captureDiagnosticLogs(t)
	RecordDatabaseOutcome(context.Background(), "db_query_lifecycle", 249*time.Millisecond, false)
	if logs.Len() != 0 {
		t.Fatal("normal database operation logged")
	}
	RecordDatabaseOutcome(context.Background(), "db_query_lifecycle", 250*time.Millisecond, false)
	RecordDatabaseOutcome(context.Background(), "db_reset", time.Millisecond, true)
	if strings.Count(logs.String(), "operation diagnostic") != 2 || !strings.Contains(logs.String(), `"phase":"db_query_lifecycle"`) || !strings.Contains(logs.String(), `"phase":"db_reset"`) {
		t.Fatal(logs.String())
	}
}

func TestFastQueriesAccumulateForSlowRequest(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 11 {
			RecordDatabaseOutcome(r.Context(), "db_query_lifecycle", 200*time.Millisecond, false)
		}
		RecordOutcome(r.Context(), "server", 200, 2200*time.Millisecond, false)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/assets/public/0123456789abcdef0123456789abcdef?secret=private", nil))
	if strings.Count(logs.String(), `"phase":"server"`) != 1 || !strings.Contains(logs.String(), `"db_query_lifecycle":{"count":11,"total_ms":2200,"max_ms":200`) {
		t.Fatal(logs.String())
	}
	if strings.Contains(logs.String(), "private") {
		t.Fatal("private query leaked")
	}
}

func TestProductionTextSummaryHasNamedPhaseFields(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	ctx := observeRequest(context.Background(), httptest.NewRequest("GET", "/", nil))
	for range 6 {
		RecordDatabaseOutcome(ctx, "db_query_lifecycle", 200*time.Millisecond, false)
	}
	RecordOutcome(ctx, "server", 200, 2200*time.Millisecond, false)
	for _, want := range []string{"phase_totals.db_query_lifecycle.count=6", "phase_totals.db_query_lifecycle.total_ms=1200", "phase_totals.db_query_lifecycle.max_ms=200"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatal(logs.String())
		}
	}
	if logs.Len() > 4096 || strings.Count(logs.String(), "operation diagnostic") != 1 {
		t.Fatal("unbounded or duplicate summary")
	}
	t.Logf("summary_records=1 bytes=%d", logs.Len())
}
