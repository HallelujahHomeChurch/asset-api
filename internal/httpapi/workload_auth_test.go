package httpapi

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hhc/asset-api/internal/config"

	"github.com/golang-jwt/jwt/v5"
)

func TestWorkloadHeaderDoesNotFallBackWhenVerifierDisabled(t *testing.T) {
	h := &Handler{
		allowedCallers: map[string]bool{"hhc-web-api": true},
		appAPIToken:    "fixture-token",
		callerConfig:   CallerConfig{ReaderCallerAppID: "api-gateway"},
	}
	accepted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, test := range []struct {
		name, legacyCaller, daprCaller string
		handler                        http.Handler
	}{
		{"private HTTPS", "hhc-web-api", "", h.internal(accepted)},
		{"private Dapr", "", "hhc-web-api", h.internal(accepted)},
		{"gateway HTTPS", "api-gateway", "", h.gatewayIngress(accepted)},
		{"gateway Dapr reader", "", "api-gateway", h.collectionReader(accepted)},
		{"gateway Dapr ticket", "", "api-gateway", h.collectionTicket(accepted)},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/assets/collections", nil)
			r.Header.Set("X-HHC-Workload-Authorization", "Bearer synthetic-invalid")
			r.Header.Set("X-HHC-User-ID", "reader")
			r.Header.Set("X-HHC-Token-Expires-At", "4102444800")
			if test.legacyCaller != "" {
				r.Header.Set("X-HHC-Caller-App-Id", test.legacyCaller)
			}
			if test.daprCaller != "" {
				r.Header.Set("Dapr-Caller-App-Id", test.daprCaller)
				r.Header.Set("Dapr-Api-Token", "fixture-token")
			}
			w := httptest.NewRecorder()
			test.handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestVerifiedWorkloadAdmissionKeepsPrincipalOwnerAndReaderSeparate(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	check := func(w http.ResponseWriter, r *http.Request) {
		principal, _ := r.Context().Value(workloadPrincipalKey{}).(string)
		w.Header().Set("Test-Principal", principal)
		w.Header().Set("Test-Service", requestService(r))
		w.Header().Set("Test-Owner", authenticatedCaller(r))
		w.Header().Set("Test-User", collectionReaderSubject(r).UserID)
		w.WriteHeader(http.StatusNoContent)
	}
	tests := []struct {
		name, principal, caller, owner, reader string
		headers                                map[string][]string
		change                                 func(jwt.MapClaims)
		signing                                jwt.SigningMethod
		key                                    *rsa.PrivateKey
		gateway, dapr                          bool
		want                                   int
	}{
		{name: "CMS verified", principal: "cms", caller: "hhc-web-api", owner: "hhc-web-api", want: 204},
		{name: "worker owner delegation", principal: "watermark", caller: "hhc-web-bulletin-worker", owner: "hhc-web-api", want: 204},
		{name: "extractor keeps distinct principal", principal: "extractor", caller: "hhc-web-api", owner: "hhc-web-api", want: 204},
		{name: "gateway reader", principal: "gateway", reader: "member", gateway: true, want: 204},
		{name: "gateway without member", principal: "gateway", gateway: true, want: 401},
		{name: "gateway cannot own assets", principal: "gateway", want: 403},
		{name: "CMS cannot become a reader", principal: "cms", reader: "member", gateway: true, want: 403},
		{name: "bad signature", principal: "cms", key: fixture.rotatedKey, want: 401},
		{name: "non RSA algorithm", principal: "cms", signing: jwt.SigningMethodHS256, want: 401},
		{name: "unknown principal", principal: "unknown", want: 403},
		{name: "missing principal", principal: "cms", change: func(c jwt.MapClaims) { delete(c, "oid") }, want: 403},
		{name: "wrong issuer", principal: "cms", change: func(c jwt.MapClaims) { c["iss"] = "https://other.example" }, want: 401},
		{name: "wrong audience", principal: "cms", change: func(c jwt.MapClaims) { c["aud"] = "wrong" }, want: 401},
		{name: "multiple audiences", principal: "cms", change: func(c jwt.MapClaims) { c["aud"] = []string{"asset-fixture", "other"} }, want: 401},
		{name: "expired", principal: "cms", change: func(c jwt.MapClaims) { c["exp"] = fixture.now.Unix() }, want: 401},
		{name: "missing expiry", principal: "cms", change: func(c jwt.MapClaims) { delete(c, "exp") }, want: 401},
		{name: "not yet valid", principal: "cms", change: func(c jwt.MapClaims) { c["nbf"] = fixture.now.Add(time.Second).Unix() }, want: 401},
		{name: "missing not before", principal: "cms", change: func(c jwt.MapClaims) { delete(c, "nbf") }, want: 401},
		{name: "delegated user token", principal: "cms", change: func(c jwt.MapClaims) { c["scp"] = "read" }, want: 401},
		{name: "mixed Dapr and token", principal: "cms", dapr: true, want: 401},
		{name: "wrong legacy alias", principal: "cms", headers: map[string][]string{"X-HHC-Caller-App-Id": {"account-api"}}, want: 401},
		{name: "matching legacy alias", principal: "cms", caller: "hhc-web-api", owner: "hhc-web-api", headers: map[string][]string{"X-HHC-Caller-App-Id": {"hhc-web-api"}}, want: 204},
		{name: "duplicate legacy alias", principal: "cms", headers: map[string][]string{"X-HHC-Caller-App-Id": {"hhc-web-api", "hhc-web-api"}}, want: 401},
		{name: "duplicate workload headers", principal: "cms", headers: map[string][]string{"X-HHC-Workload-Authorization": {"Bearer another"}}, want: 401},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := fixture.claims(test.principal)
			if test.change != nil {
				test.change(claims)
			}
			key := test.key
			if key == nil {
				key = fixture.firstKey
			}
			r := httptest.NewRequest(http.MethodGet, "/api/assets/collections", nil)
			r.Header.Set("X-HHC-Workload-Authorization", fixture.bearer(t, "first", key, test.signing, claims))
			r.Header.Set("Authorization", "Bearer end-user-is-separate")
			for name, values := range test.headers {
				for _, value := range values {
					r.Header.Add(name, value)
				}
			}
			if test.dapr {
				r.Header.Set("Dapr-Caller-App-Id", "hhc-web-api")
				r.Header.Set("Dapr-Api-Token", "fixture-token")
			}
			if test.reader != "" {
				r.Header.Set("X-HHC-User-ID", test.reader)
				r.Header.Set("X-HHC-Token-Expires-At", "4102444800")
			}
			handler := h.internal(http.HandlerFunc(check))
			if test.gateway {
				handler = h.gatewayIngress(h.collectionReader(http.HandlerFunc(check)))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", w.Code, test.want, w.Body.String())
			}
			if test.want == 204 && (w.Header().Get("Test-Principal") != test.principal ||
				w.Header().Get("Test-Service") != test.caller || w.Header().Get("Test-Owner") != test.owner || w.Header().Get("Test-User") != test.reader) {
				t.Fatalf("identity = %v", w.Header())
			}
			if test.want != 204 && (w.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(w.Body.String(), "synthetic")) {
				t.Fatalf("unsafe error: headers=%v body=%s", w.Header(), w.Body.String())
			}
		})
	}
	if got := fixture.fetches.Load(); got != 1 {
		t.Fatalf("fetched keys %d times, want 1", got)
	}
}

func TestWorkloadKeyRotationAndOutageAreBounded(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	check := h.internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	route := func(kid string, key *rsa.PrivateKey, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/priv/assets/item", nil)
		r.Header.Set("X-HHC-Workload-Authorization", fixture.bearer(t, kid, key, nil, fixture.claims("cms")))
		r.Header.Set("X-HHC-Caller-App-Id", "hhc-web-api")
		w := httptest.NewRecorder()
		check.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status = %d, want %d, body = %s", w.Code, want, w.Body.String())
		}
		if want == 503 && (strings.Contains(w.Body.String(), "synthetic upstream details") || w.Header().Get("Cache-Control") != "private, no-store") {
			t.Fatal("unsafe dependency failure")
		}
	}
	route("first", fixture.firstKey, 204)
	fixture.rotated.Store(true)
	route("rotated", fixture.rotatedKey, 401)
	if got := fixture.fetches.Load(); got != 1 {
		t.Fatalf("rotation before cooldown fetched %d times", got)
	}
	fixture.now = fixture.now.Add(time.Minute)
	route("rotated", fixture.rotatedKey, 204)
	route("first", fixture.firstKey, 401)
	fixture.outage.Store(true)
	route("rotated", fixture.rotatedKey, 204)
	fixture.now = fixture.now.Add(10 * time.Minute)
	route("rotated", fixture.rotatedKey, 503)
	route("rotated", fixture.rotatedKey, 503)
	if got := fixture.fetches.Load(); got != 3 {
		t.Fatalf("outage fetched %d times, want 3", got)
	}
	fixture.outage.Store(false)
	fixture.now = fixture.now.Add(time.Minute)
	route("rotated", fixture.rotatedKey, 204)
}

func TestWorkloadKeyRefreshCoalescesConcurrentRequests(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	handler := h.internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	header := fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims("cms"))
	start := make(chan struct{})
	var requests sync.WaitGroup
	for range 16 {
		requests.Go(func() {
			<-start
			r := httptest.NewRequest(http.MethodGet, "/priv/assets/item", nil)
			r.Header.Set(workloadHeader, header)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}
	close(start)
	requests.Wait()
	if got := fixture.fetches.Load(); got != 1 {
		t.Fatalf("fetched keys %d times, want 1", got)
	}
}

func TestWorkloadKeyEndpointRejectsRedirectAndOversizedBody(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	for _, test := range []struct {
		name  string
		serve http.HandlerFunc
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fixture.server.URL+"/keys", http.StatusTemporaryRedirect)
		}},
		{"oversized JWKS", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat(" ", workloadJWKSSize+1)))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(test.serve)
			defer server.Close()
			h := fixture.handler()
			settings := h.workloadAuth.config
			settings.JWKSURL = server.URL
			h.workloadAuth = newWorkloadVerifier(settings, server.Client().Transport, func() time.Time { return fixture.now })
			r := httptest.NewRequest(http.MethodGet, "/priv/assets/item", nil)
			r.Header.Set(workloadHeader, fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims("cms")))
			w := httptest.NewRecorder()
			h.internal(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("admitted invalid JWKS") })).ServeHTTP(w, r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}
	if got := fixture.fetches.Load(); got != 0 {
		t.Fatalf("redirect reached the other key endpoint %d times", got)
	}
}

func TestWorkloadLegacyRetiresPerCallerOnBothTransports(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	accepted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, test := range []struct {
		caller string
		https  bool
		want   int
	}{
		{"hhc-web-api", true, 403},
		{"hhc-web-api", false, 403},
		{"api-gateway", true, 403},
		{"api-gateway", false, 403},
		{"account-api", true, 204},
		{"account-api", false, 204},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/assets/collections", nil)
		r.Header.Set("X-HHC-User-ID", "member")
		r.Header.Set("X-HHC-Token-Expires-At", "4102444800")
		handler := h.internal(accepted)
		if test.https {
			r.Header.Set("X-HHC-Caller-App-Id", test.caller)
			if test.caller == "api-gateway" {
				handler = h.gatewayIngress(h.collectionReader(accepted))
			}
		} else {
			r.Header.Set("Dapr-Caller-App-Id", test.caller)
			r.Header.Set("Dapr-Api-Token", "fixture-token")
			if test.caller == "api-gateway" {
				handler = h.collectionReader(accepted)
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("caller=%s https=%v status=%d want=%d", test.caller, test.https, w.Code, test.want)
		}
	}
	if got := fixture.fetches.Load(); got != 0 {
		t.Fatalf("legacy requests fetched JWKS %d times", got)
	}
}

type workloadAuthFixture struct {
	firstKey, rotatedKey *rsa.PrivateKey
	server               *httptest.Server
	now                  time.Time
	fetches              atomic.Int32
	rotated, outage      atomic.Bool
}

func newWorkloadAuthFixture(t *testing.T) *workloadAuthFixture {
	t.Helper()
	f := &workloadAuthFixture{now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	var err error
	f.firstKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.rotatedKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetches.Add(1)
		if f.outage.Load() || r.URL.Path != "/keys" {
			http.Error(w, "synthetic upstream details", http.StatusServiceUnavailable)
			return
		}
		kid, key := "first", f.firstKey
		if f.rotated.Load() {
			kid, key = "rotated", f.rotatedKey
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": kid, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *workloadAuthFixture) handler() *Handler {
	h := &Handler{
		allowedCallers: map[string]bool{"account-api": true, "hhc-web-api": true},
		appAPIToken:    "fixture-token",
		callerConfig:   CallerConfig{ReaderCallerAppID: "api-gateway"},
	}
	h.workloadAuth = newWorkloadVerifier(config.WorkloadAuth{
		Issuer: f.server.URL + "/issuer", Audience: "asset-fixture", JWKSURL: f.server.URL + "/keys", PrincipalClaim: "oid",
		Principals: map[string]string{
			"cms": "hhc-web-api", "extractor": "hhc-web-api", "watermark": "hhc-web-bulletin-worker", "gateway": "api-gateway",
		},
		DisableLegacyCallers: []string{"hhc-web-api", "api-gateway"},
	}, f.server.Client().Transport, func() time.Time { return f.now })
	return h
}

func (f *workloadAuthFixture) claims(principal string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": f.server.URL + "/issuer", "aud": "asset-fixture",
		"nbf": f.now.Add(-time.Minute).Unix(), "exp": f.now.Add(time.Minute).Unix(), "oid": principal,
	}
}

func (f *workloadAuthFixture) bearer(t *testing.T, kid string, key *rsa.PrivateKey, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	if method == nil {
		method = jwt.SigningMethodRS256
	}
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	var signingKey any = key
	if method.Alg() == jwt.SigningMethodHS256.Alg() {
		signingKey = []byte{1, 2, 3, 4}
	}
	raw, err := token.SignedString(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + raw
}
