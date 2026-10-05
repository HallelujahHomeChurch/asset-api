package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestRecordingCoverRoutesFailClosed(t *testing.T) {
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, tc := range []struct {
		method, path, caller string
		want                 int
	}{
		{"GET", "/priv/recording-packages/p/covers?recordingId=r", "account-api", 403},
		{"GET", "/priv/recording-packages/p/covers?recordingId=r", "hhc-web-api", 503},
		{"POST", "/priv/recording-packages/p/cover-uploads?recordingId=r", "hhc-web-api", 503},
		{"GET", "/priv/recording-packages/p/covers/x-1/content?recordingId=r", "account-api", 403},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}
