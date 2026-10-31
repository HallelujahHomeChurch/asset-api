package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDerivedBulletinRequiresExactAuthenticatedWorker(t *testing.T) {
	for _, caller := range []string{"hhc-web-api", "api-gateway", "account-api", "hhc-web-bulletin-worker"} {
		for _, token := range []string{"", "wrong", "app-token"} {
			h := New(nil, nil, map[string]bool{"hhc-web-api": true, "api-gateway": true, "account-api": true}, false, "app-token", WorkloadAuthConfig{}, nil)
			r := httptest.NewRequest(http.MethodPost, "/priv/assets/bulletin-derivatives/asset/complete", strings.NewReader(`{"unexpected":true}`))
			r.Header.Set("Dapr-Caller-App-Id", caller)
			r.Header.Set("dapr-api-token", token)
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			want := http.StatusForbidden
			if caller == "hhc-web-bulletin-worker" && token == "app-token" {
				want = http.StatusBadRequest
			}
			if w.Code != want {
				t.Fatalf("caller=%s authenticated=%v status=%d want=%d", caller, token == "app-token", w.Code, want)
			}
		}
	}
}
