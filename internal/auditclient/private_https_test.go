package auditclient

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"hhc/asset-api/internal/telemetry"
)

func TestAuditProbeDiagnosticIsDistinctFromAppend(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_event"}`)
	}))
	defer srv.Close()
	client, err := NewPrivateHTTPS("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	client.endpoint = srv.URL + "/private-sensitive-path?token=secret-query"
	client.http.Transport = telemetry.AuditTransport{Base: srv.Client().Transport}
	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))
	if err := client.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	probe := logs.String()
	if !strings.Contains(probe, `"operation":"audit_auth_probe"`) || !strings.Contains(probe, `"status":400`) || !strings.Contains(probe, `"sampled":false`) || !strings.Contains(probe, tid.String()) {
		t.Fatal("probe diagnostic missing bounded unsampled correlation")
	}
	logs.Reset()
	if result := client.Append(ctx, []byte("sensitive-payload")); result.Result != "dead_letter" {
		t.Fatal("append rejection changed")
	}
	appendLog := logs.String()
	if !strings.Contains(appendLog, `"operation":"audit_append"`) || strings.Contains(appendLog, "audit_auth_probe") {
		t.Fatal("append misclassified as probe")
	}
	for _, forbidden := range []string{"test-secret", "private-sensitive-path", "secret-query", "sensitive-payload"} {
		if strings.Contains(probe+appendLog, forbidden) {
			t.Fatal("sensitive request data logged")
		}
	}
}

func TestPrivateHTTPSAppendAndProbe(t *testing.T) {
	client, err := NewPrivateHTTPS("test-token")
	if err != nil {
		t.Fatal(err)
	}
	if client.endpoint != "https://audit-log.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io/priv/audit/events" {
		t.Fatalf("unexpected destination %q", client.endpoint)
	}
	status := http.StatusBadRequest
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/priv/audit/events" ||
			r.Header.Get("X-HHC-Audit-Caller") != "asset-api" || r.Header.Get("X-Audit-Token") != "test-token" ||
			r.Header.Get("Dapr-Caller-App-Id") != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid private audit request")
		}
		body, _ := io.ReadAll(r.Body)
		if (status == http.StatusBadRequest && len(body) != 0) || (status == http.StatusCreated && string(body) != "{}") {
			t.Error("wrong audit payload")
		}
		w.WriteHeader(status)
		if status == http.StatusBadRequest {
			_, _ = io.WriteString(w, `{"error":"invalid_event"}`)
		}
	}))
	defer srv.Close()
	client.endpoint = srv.URL + "/priv/audit/events"
	client.http.Transport = srv.Client().Transport
	ctx := context.Background()
	if err := client.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := client.Verify(canceled); err == nil {
		t.Fatal("canceled probe passed")
	}
	status = http.StatusUnauthorized
	if err := client.Verify(ctx); err == nil {
		t.Fatal("unauthorized probe passed")
	}
	if result := client.Append(ctx, []byte("{}")); result.Result != "retry" {
		t.Fatalf("auth failure lost retry: %v", result)
	}
	status = http.StatusCreated
	if result := client.Append(ctx, []byte("{}")); result.Result != "accepted" {
		t.Fatalf("append = %v", result)
	}
}
