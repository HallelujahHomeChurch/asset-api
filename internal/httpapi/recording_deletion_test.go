package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type deletionFixture struct{ calls int }

func (d *deletionFixture) DeleteRecording(_ context.Context, id string) error { d.calls++; return nil }
func TestRecordingDeleteIsRestrictedToOwningCMS(t *testing.T) {
	d := &deletionFixture{}
	svc := assets.NewRecordingPackageService(&httpPackageRepo{}, httpPackageObjects{}, time.Now)
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingPackages(svc).WithRecordingDeletion(d).Routes()
	for _, tc := range []struct {
		caller string
		want   int
	}{{"", 403}, {"account-api", 403}, {"hhc-web-api", 204}} {
		r := httptest.NewRequest(http.MethodDelete, "/priv/recordings/recording-a", nil)
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.caller, w.Code, w.Body)
		}
	}
	if d.calls != 1 {
		t.Fatalf("unauthorized caller deleted media: %d", d.calls)
	}
}
