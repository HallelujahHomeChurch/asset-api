package auditclient

import (
	"context"
	"hhc/asset-api/internal/diagnostics"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientForwardsHTTPCorrelation(t *testing.T) {
	const trace = "00-11111111111111111111111111111111-2222222222222222-01"
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("X-HHC-Request-ID") != "request-1" || r.Header.Get("traceparent") != trace {
			t.Error("correlation missing")
		}
		w.WriteHeader(503)
	}))
	defer server.Close()
	ctx, _ := diagnostics.HTTPContext(context.Background(), http.Header{"X-Hhc-Request-Id": {"request-1"}, "Traceparent": {trace}})
	_ = (&Client{http: server.Client(), endpoint: server.URL}).Append(ctx, []byte(`{}`))
	if !called {
		t.Fatal("no request")
	}
}
