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
			h := New(nil, nil, map[string]bool{"hhc-web-api": true, "api-gateway": true, "account-api": true}, false, "app-token", CallerConfig{}, nil)
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

func derivedBulletinStatus(t *testing.T, headers map[string]string) int {
	t.Helper()
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "api-gateway": true, "hhc-line-function-bot": true}, false, "app-token", CallerConfig{}, nil)
	r := httptest.NewRequest(http.MethodPost, "/priv/assets/bulletin-derivatives/asset/complete", strings.NewReader(`{"unexpected":true}`))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	return w.Code
}

// Retired platform principal headers never grant worker privileges.
func TestDerivedBulletinDaprIdentityTakesPrecedenceOverPrincipalHeader(t *testing.T) {
	forged := encodedPrincipal(t, map[string]string{"appid": "watermark-client", "roles": "Asset.Invoke"})
	for _, caller := range []string{"hhc-web-api", "api-gateway", "hhc-line-function-bot"} {
		if got := derivedBulletinStatus(t, map[string]string{"dapr-api-token": "app-token", "Dapr-Caller-App-Id": caller, "X-MS-CLIENT-PRINCIPAL": forged}); got != http.StatusForbidden {
			t.Fatalf("dapr caller %s with forged worker principal status=%d", caller, got)
		}
	}
	if got := derivedBulletinStatus(t, map[string]string{"dapr-api-token": "app-token", "Dapr-Caller-App-Id": "hhc-web-bulletin-worker"}); got != http.StatusBadRequest {
		t.Fatalf("dapr worker status=%d", got)
	}
	// A spoofed Dapr caller header without the app-channel token is not Dapr.
	if got := derivedBulletinStatus(t, map[string]string{"Dapr-Caller-App-Id": "hhc-web-bulletin-worker"}); got != http.StatusForbidden {
		t.Fatalf("tokenless dapr worker header status=%d", got)
	}
	if got := derivedBulletinStatus(t, map[string]string{"dapr-api-token": "wrong", "Dapr-Caller-App-Id": "hhc-web-bulletin-worker"}); got != http.StatusForbidden {
		t.Fatalf("wrong-token dapr worker header status=%d", got)
	}
}

// The worker acts for CMS on every other private route, exactly like Dapr.
func TestWatermarkInternalCallerActsAsCMSOwnerOnPrivateRoutes(t *testing.T) {
	h := &Handler{allowedCallers: map[string]bool{"hhc-web-api": true}, appAPIToken: "app-token", callerConfig: CallerConfig{}}
	handler := h.internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticatedCaller(r) != "hhc-web-api" || requestService(r) != "hhc-web-bulletin-worker" {
			t.Fatalf("caller=%q service=%q", authenticatedCaller(r), requestService(r))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/priv/assets/owned-asset", nil)
	r.Header.Set("X-HHC-Caller-App-Id", "hhc-web-bulletin-worker")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}
}
