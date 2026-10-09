package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestLiveCoverRoutesFailClosed(t *testing.T) {
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, tc := range []struct {
		method, path, caller string
		want                 int
	}{
		{"POST", "/priv/recording-live-covers/defaults/uploads", "account-api", 403},
		{"POST", "/priv/recording-live-covers/defaults/uploads", "hhc-web-api", 503},
		{"GET", "/priv/recording-live-covers/defaults/uploads/one", "hhc-web-api", 503},
		{"GET", "/priv/recording-live-covers/defaults/uploads/one/content", "account-api", 403},
		{"POST", "/priv/recording-live-covers/capture/auto", "account-api", 403},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}
