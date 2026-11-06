package auditclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
