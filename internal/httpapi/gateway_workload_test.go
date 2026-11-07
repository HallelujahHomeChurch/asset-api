package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtectedReadersAcceptOnlyGatewayCaller(t *testing.T) {
	auth := CallerConfig{ReaderCallerAppID: "api-gateway"}
	h := &Handler{appAPIToken: "test-app-token", callerConfig: auth, allowedCallers: map[string]bool{"account-api": true}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, caller := range []string{"api-gateway", "account-api", "unknown"} {
		t.Run(caller, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/_gateway/api/assets/collections", nil)
			r.Header.Set("X-HHC-Caller-App-Id", caller)
			reader, private := http.StatusForbidden, http.StatusForbidden
			if caller == "api-gateway" {
				reader = http.StatusUnauthorized
			}
			if caller == "account-api" {
				private = http.StatusNoContent
			}
			for _, target := range []struct {
				handler http.Handler
				want    int
			}{
				{h.gatewayIngress(h.collectionReader(ok)), reader}, {h.internal(ok), private},
				{h.collectionReader(ok), http.StatusForbidden}, {h.Routes(), reader},
			} {
				w := httptest.NewRecorder()
				target.handler.ServeHTTP(w, r)
				if w.Code != target.want {
					t.Fatalf("status=%d want=%d", w.Code, target.want)
				}
			}
		})
	}
}
