package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyRecordingEndpointsCannotAcceptWork(t *testing.T) {
	handler := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/priv/recording-uploads"},
		{"GET", "/priv/recording-uploads/session"},
		{"GET", "/priv/recording-uploads/session/parts"},
		{"POST", "/priv/recording-uploads/session/parts/1"},
		{"POST", "/priv/recording-uploads/session/complete"},
		{"DELETE", "/priv/recording-uploads/session"},
		{"GET", "/priv/recording-assets/version"},
		{"POST", "/priv/recording-assets/version/grants"},
		{"DELETE", "/priv/recording-assets/version"},
	} {
		r := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(`{}`))
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("retired %s %s returned %d, want 404", endpoint.method, endpoint.path, w.Code)
		}
	}
}
