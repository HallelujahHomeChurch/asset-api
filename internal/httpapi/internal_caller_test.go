package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedInternalCallerHeader(t *testing.T) {
	h := &Handler{allowedCallers: map[string]bool{"account-api": true, "hhc-web-api": true, "hhc-line-function-bot": true}, appAPIToken: "token", callerConfig: CallerConfig{ReaderCallerAppID: "api-gateway"}}
	for _, tc := range []struct {
		name   string
		values []string
		dapr   string
		want   string
	}{
		{"account", []string{"account-api"}, "", "account-api"},
		{"cms", []string{"hhc-web-api"}, "", "hhc-web-api"},
		{"line", []string{"hhc-line-function-bot"}, "", "hhc-line-function-bot"},
		{"worker", []string{"hhc-web-bulletin-worker"}, "", "hhc-web-bulletin-worker"},
		{"gateway", []string{"api-gateway"}, "", "api-gateway"},
		{"missing", nil, "", ""}, {"unknown", []string{"unknown"}, "", ""},
		{"empty", []string{""}, "account-api", ""},
		{"duplicate", []string{"account-api", "account-api"}, "", ""},
		{"conflicting", []string{"account-api", "hhc-web-api"}, "", ""},
		{"comma", []string{"account-api,hhc-web-api"}, "", ""},
		{"whitespace", []string{" account-api "}, "", ""},
		{"case", []string{"Account-Api"}, "", ""},
		{"invalid-with-dapr", []string{" account-api"}, "account-api", ""},
		{"ambiguous-dapr", []string{"hhc-web-api"}, "account-api", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://asset.internal/priv/assets/item", nil)
			if tc.values != nil {
				r.Header["X-Hhc-Caller-App-Id"] = tc.values
			}
			if tc.dapr != "" {
				r.Header.Set("Dapr-Caller-App-Id", tc.dapr)
				r.Header.Set("dapr-api-token", "token")
			}
			if got := h.authenticatedService(r); got != tc.want {
				t.Fatalf("caller=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestTrustedGatewayKeepsReaderAndPrivateBoundaries(t *testing.T) {
	h := &Handler{allowedCallers: map[string]bool{"account-api": true}, callerConfig: CallerConfig{ReaderCallerAppID: "api-gateway"}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, caller := range []string{"api-gateway", "account-api"} {
		r := httptest.NewRequest("GET", "https://asset.internal/_gateway/api/assets/collections", nil)
		r.Header.Set("X-HHC-Caller-App-Id", caller)
		for _, target := range []struct {
			handler http.Handler
			want    int
		}{
			{h.gatewayIngress(h.collectionReader(ok)), map[string]int{"api-gateway": 401, "account-api": 403}[caller]},
			{h.internal(ok), map[string]int{"api-gateway": 403, "account-api": 204}[caller]},
		} {
			w := httptest.NewRecorder()
			target.handler.ServeHTTP(w, r)
			if w.Code != target.want {
				t.Fatalf("%s status=%d want=%d", caller, w.Code, target.want)
			}
		}
	}
}

func TestTrustedGatewayUnicodeContextDoesNotRequirePrincipal(t *testing.T) {
	h := &Handler{callerConfig: CallerConfig{ReaderCallerAppID: "api-gateway"}}
	server := httptest.NewTLSServer(h.gatewayIngress(h.collectionReader(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := collectionReaderSubject(r).UserID; got != "使用者" {
			t.Errorf("user=%q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))))
	defer server.Close()
	r, err := http.NewRequest(http.MethodGet, server.URL+"/_gateway/api/assets/collections", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-HHC-Caller-App-Id", "api-gateway")
	r.Header.Set("X-HHC-User-ID", "使用者")
	r.Header.Set("X-HHC-User-Name", "測試使用者")
	r.Header.Set("X-HHC-Token-Expires-At", "4102444800")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func TestTrustedHeaderPreservesWorkerOwnerAndDerivativeRestriction(t *testing.T) {
	h := &Handler{allowedCallers: map[string]bool{"hhc-web-api": true}}
	r := httptest.NewRequest(http.MethodGet, "https://asset.internal/priv/assets/item", nil)
	r.Header.Set("X-HHC-Caller-App-Id", "hhc-web-bulletin-worker")
	w := httptest.NewRecorder()
	h.internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticatedCaller(r) != "hhc-web-api" || requestService(r) != "hhc-web-bulletin-worker" {
			t.Fatal("worker owner alias changed")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}
	for _, caller := range []string{"hhc-web-api", "hhc-line-function-bot", "hhc-web-bulletin-worker"} {
		want := http.StatusForbidden
		if caller == "hhc-web-bulletin-worker" {
			want = http.StatusBadRequest
		}
		if got := derivedBulletinStatus(t, map[string]string{"X-HHC-Caller-App-Id": caller}); got != want {
			t.Fatalf("caller=%s status=%d want=%d", caller, got, want)
		}
	}
}

func TestTrustedGatewayStillRequiresMemberScope(t *testing.T) {
	h := &Handler{callerConfig: CallerConfig{ReaderCallerAppID: "api-gateway"}}
	handler := h.gatewayIngress(h.personalReader(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	for _, scope := range []string{"", "unrelated:scope", "presenter:cloud:use"} {
		r := httptest.NewRequest(http.MethodGet, "https://asset.internal/_gateway/api/assets/personal", nil)
		r.Header.Set("X-HHC-Caller-App-Id", "api-gateway")
		r.Header.Set("X-HHC-User-ID", "member")
		r.Header.Set("X-HHC-Token-Expires-At", "4102444800")
		r.Header.Set("X-HHC-Scopes", scope)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := http.StatusForbidden
		if scope == "presenter:cloud:use" {
			want = http.StatusNoContent
		}
		if w.Code != want {
			t.Fatalf("scope=%q status=%d want=%d", scope, w.Code, want)
		}
	}
}

func TestLegacyPrincipalNeverIdentifiesCaller(t *testing.T) {
	h := &Handler{allowedCallers: map[string]bool{"account-api": true}, callerConfig: CallerConfig{ReaderCallerAppID: "api-gateway"}}
	r := httptest.NewRequest(http.MethodGet, "https://asset.internal/priv/assets/item", nil)
	r.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, map[string]string{"tid": "tenant", "iss": "issuer", "aud": "audience", "roles": "role", "appid": "client", "oid": "object"}))
	if got := h.authenticatedService(r); got != "" {
		t.Fatalf("legacy principal identified %q", got)
	}
}

func TestLegacyReaderRejectsMixedCallerHeaders(t *testing.T) {
	h := &Handler{appAPIToken: "token", callerConfig: CallerConfig{ReaderCallerAppID: "api-gateway"}}
	r := httptest.NewRequest(http.MethodGet, "https://asset.internal/api/assets/collections", nil)
	r.Header.Set("Dapr-Caller-App-Id", "api-gateway")
	r.Header.Set("dapr-api-token", "token")
	if h.gatewayReader(r) != nil {
		t.Fatal("legacy reader rejected")
	}
	for _, header := range []string{"api-gateway", " api-gateway", ""} {
		r.Header.Set("X-HHC-Caller-App-Id", header)
		if h.gatewayReader(r) == nil {
			t.Fatalf("mixed reader header %q accepted", header)
		}
	}
}
