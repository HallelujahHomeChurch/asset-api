package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtectedReadersAcceptOnlyGatewayWorkload(t *testing.T) {
	auth := WorkloadAuthConfig{
		TenantID: "tenant", Issuer: "https://sts.windows.net/tenant/", Audience: "api://asset-api", RequiredRole: "Asset.Invoke",
		ReaderCallerAppID: "api-gateway",
		Callers: map[string]WorkloadCaller{
			"gateway-client": {ObjectID: "gateway-object", Service: "api-gateway"},
			"account-client": {ObjectID: "account-object", Service: "account-api"},
		},
	}
	h := &Handler{appAPIToken: "test-app-token", workloadAuth: auth, allowedCallers: map[string]bool{"account-api": true}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		name, client, object, role, dapr string
		reader, internal                 int
	}{
		{"Gateway", "gateway-client", "gateway-object", "Asset.Invoke", "", 401, 403},
		{"Account", "account-client", "account-object", "Asset.Invoke", "", 403, 204},
		{"Invalid object", "gateway-client", "account-object", "Asset.Invoke", "", 403, 403},
		{"Invalid role", "gateway-client", "gateway-object", "other", "", 403, 403},
		{"Dapr wins", "gateway-client", "gateway-object", "Asset.Invoke", "account-api", 403, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/assets/collections", nil)
			r.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, map[string]string{
				"tid": auth.TenantID, "iss": auth.Issuer, "aud": auth.Audience, "appid": tc.client, "oid": tc.object, "roles": tc.role,
			}))
			if tc.dapr != "" {
				r.Header.Set("Dapr-Caller-App-Id", tc.dapr)
				r.Header.Set("dapr-api-token", "test-app-token")
			}
			for _, target := range []struct {
				handler http.Handler
				want    int
			}{
				{h.gatewayWorkload(h.collectionReader(ok)), tc.reader}, {h.internal(ok), tc.internal},
				{h.collectionReader(ok), http.StatusForbidden},
			} {
				w := httptest.NewRecorder()
				target.handler.ServeHTTP(w, r)
				if w.Code != target.want {
					t.Fatalf("status = %d, want %d", w.Code, target.want)
				}
			}
			r.URL.Path = "/_gateway/api/assets/collections"
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			if w.Code != tc.reader {
				t.Fatalf("private route status = %d, want %d", w.Code, tc.reader)
			}
		})
	}
}
