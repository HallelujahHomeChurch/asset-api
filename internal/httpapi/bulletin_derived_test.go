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

func watermarkWorkloadAuth() WorkloadAuthConfig {
	return WorkloadAuthConfig{TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://sts.windows.net/tenant-1/", RequiredRole: "Asset.Invoke", Callers: map[string]WorkloadCaller{
		"watermark-client": {ObjectID: "watermark-object", Service: "hhc-web-bulletin-worker"},
		"extractor-client": {ObjectID: "extractor-object", Service: "hhc-web-api"},
		"line-client":      {ObjectID: "line-object", Service: "hhc-line-function-bot"},
	}}
}

func watermarkClaims() map[string]string {
	auth := watermarkWorkloadAuth()
	return map[string]string{"tid": auth.TenantID, "iss": auth.Issuer, "aud": auth.Audience, "appid": "watermark-client", "oid": "watermark-object", "roles": auth.RequiredRole}
}

func derivedBulletinStatus(t *testing.T, headers map[string]string) int {
	t.Helper()
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "api-gateway": true, "hhc-line-function-bot": true}, false, "app-token", watermarkWorkloadAuth(), nil)
	r := httptest.NewRequest(http.MethodPost, "/priv/assets/bulletin-derivatives/asset/complete", strings.NewReader(`{"unexpected":true}`))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	return w.Code
}

// An authenticated caller reaches body validation (400); others stop at 403.
func TestDerivedBulletinAcceptsWatermarkWorkloadIdentity(t *testing.T) {
	if got := derivedBulletinStatus(t, map[string]string{"X-MS-CLIENT-PRINCIPAL": encodedPrincipal(t, watermarkClaims())}); got != http.StatusBadRequest {
		t.Fatalf("watermark workload status=%d", got)
	}
}

func TestDerivedBulletinRejectsOtherOrForgedWorkloadPrincipals(t *testing.T) {
	cases := map[string]map[string]string{}
	for key, value := range map[string]string{"appid": "extractor-client", "oid": "extractor-object", "roles": "Other.Role", "aud": "api://other", "tid": "tenant-2", "iss": "https://evil.example/"} {
		claims := watermarkClaims()
		claims[key] = value
		cases["wrong "+key] = claims
	}
	extractor := watermarkClaims()
	extractor["appid"], extractor["oid"] = "extractor-client", "extractor-object"
	cases["extractor identity"] = extractor
	line := watermarkClaims()
	line["appid"], line["oid"] = "line-client", "line-object"
	cases["line identity"] = line
	for name, claims := range cases {
		if got := derivedBulletinStatus(t, map[string]string{"X-MS-CLIENT-PRINCIPAL": encodedPrincipal(t, claims)}); got != http.StatusForbidden {
			t.Fatalf("%s status=%d", name, got)
		}
	}
	for name, header := range map[string]string{"not base64": "%%%", "not json": "bm90LWpzb24=", "empty": ""} {
		if got := derivedBulletinStatus(t, map[string]string{"X-MS-CLIENT-PRINCIPAL": header}); got != http.StatusForbidden {
			t.Fatalf("%s status=%d", name, got)
		}
	}
}

// Dapr reaches the app over localhost without Easy Auth, so a principal
// header on an authenticated Dapr request is caller-controlled. The Dapr
// identity must win and the forged worker principal must be ignored.
func TestDerivedBulletinDaprIdentityTakesPrecedenceOverPrincipalHeader(t *testing.T) {
	forged := encodedPrincipal(t, watermarkClaims())
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
func TestWatermarkWorkloadActsAsCMSOwnerOnPrivateRoutes(t *testing.T) {
	h := &Handler{allowedCallers: map[string]bool{"hhc-web-api": true}, appAPIToken: "app-token", workloadAuth: watermarkWorkloadAuth()}
	handler := h.internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticatedCaller(r) != "hhc-web-api" || requestService(r) != "hhc-web-bulletin-worker" {
			t.Fatalf("caller=%q service=%q", authenticatedCaller(r), requestService(r))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/priv/assets/owned-asset", nil)
	r.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, watermarkClaims()))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}
}
