package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

type retentionStoreStub struct{ calls int }

func (s *retentionStoreStub) RecordingLifecycle(context.Context, []assets.RecordingLifecycleBinding) (assets.RecordingLifecycleSnapshot, error) {
	s.calls++
	return assets.RecordingLifecycleSnapshot{}, nil
}

func (s *retentionStoreStub) GetRetentionPolicy(context.Context) (assets.RecordingRetentionPolicy, error) {
	s.calls++
	return assets.RecordingRetentionPolicy{RetentionDays: 30, Revision: 1}, nil
}
func (s *retentionStoreStub) PreviewRetentionPolicy(context.Context, int) (assets.RecordingRetentionPreview, error) {
	s.calls++
	return assets.RecordingRetentionPreview{}, nil
}
func (s *retentionStoreStub) UpdateRetentionPolicy(context.Context, assets.UpdateRecordingRetentionInput) (assets.RecordingRetentionPolicy, error) {
	s.calls++
	return assets.RecordingRetentionPolicy{}, nil
}

func TestRecordingRetentionOwnerAndHumanMutationBoundary(t *testing.T) {
	s := &retentionStoreStub{}
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).
		WithRecordingPackages(assets.NewRecordingPackageService(&httpPackageRepo{}, httpPackageObjects{}, nil)).WithRecordingRetention(s).Routes()
	for _, tc := range []struct {
		method, path, caller, actor, body string
		status                            int
	}{
		{"GET", "/priv/recordings/retention-policy", "hhc-web-api", "", "", 200},
		{"GET", "/priv/recordings/retention-policy", "account-api", "", "", 403},
		{"POST", "/priv/recordings/retention-policy/preview", "hhc-web-api", "", `{"retentionDays":14}`, 403},
		{"PUT", "/priv/recordings/retention-policy", "hhc-web-api", "", `{"retentionDays":14}`, 403},
		{"POST", "/priv/recordings/retention-policy/preview", "hhc-web-api", "11111111-1111-4111-8111-111111111111", `{"retentionDays":14}`, 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		r.Header.Set("X-HHC-Actor-ID", tc.actor)
		r.Header.Set("X-HHC-Request-ID", "retention-test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	if s.calls != 2 {
		t.Fatalf("unauthorized calls reached store: %d", s.calls)
	}
}
