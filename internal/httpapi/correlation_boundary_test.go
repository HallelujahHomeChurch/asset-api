package httpapi

import (
	"hhc/asset-api/internal/diagnostics"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiagnosticCorrelationDoesNotCreateAuditProvenance(t *testing.T) {
	for _, ids := range [][]string{nil, {"invalid value"}, {"first", "second"}, {"valid-1"}} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header["X-Hhc-Request-Id"] = ids
		var forwarded http.Header
		wrapped := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwarded = make(http.Header)
			diagnostics.ForwardHTTP(r.Context(), forwarded)
			if len(r.Header.Values("X-HHC-Request-ID")) != len(ids) {
				t.Fatal("audit headers changed")
			}
		}))
		response := httptest.NewRecorder()
		wrapped.ServeHTTP(response, req)
		id := response.Header().Get("X-HHC-Request-ID")
		if id == "" || forwarded.Get("X-HHC-Request-ID") != id {
			t.Fatal("response/context mismatch")
		}
		if len(ids) == 1 && ids[0] == "valid-1" && id != "valid-1" {
			t.Fatal("valid id changed")
		}
		if len(ids) > 1 && id == ids[0] {
			t.Fatal("duplicate header accepted")
		}
	}
}
