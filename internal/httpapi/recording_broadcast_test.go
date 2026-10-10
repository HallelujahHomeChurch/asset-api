package httpapi

import (
	"context"
	"hhc/asset-api/internal/assets"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type httpBroadcastRepo struct {
	policy assets.RecordingBroadcastRange
}

func (s *httpBroadcastRepo) SetBroadcastRange(_ context.Context, _ string, p assets.RecordingBroadcastRange) (assets.RecordingBroadcastRange, error) {
	if err := assets.ValidateBroadcastRangeChange(nil, p); err != nil {
		return p, err
	}
	s.policy = p
	return p, nil
}
func (s *httpBroadcastRepo) GetBroadcastRange(context.Context, string) (*assets.RecordingBroadcastRange, error) {
	return &s.policy, nil
}
func (s *httpBroadcastRepo) GetBroadcastProjection(context.Context, string) (assets.RecordingBroadcastProjection, error) {
	return assets.RecordingBroadcastProjection{Epoch: 1, RangeRevision: 1, State: "pending", Revision: 1}, nil
}
func TestBroadcastRoutesAreCMSOnlyAndSchemaStrict(t *testing.T) {
	repo := &httpBroadcastRepo{}
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCaptures(assets.NewRecordingCaptureService(&httpCaptureRepo{}, httpPackageObjects{}, time.Now)).WithRecordingBroadcasts(repo).Routes()
	good := `{"recordingId":"11111111-1111-4111-8111-111111111111","epoch":1,"rangeRevision":1,"startSequence":null,"endSequenceExclusive":null,"revoked":false}`
	for _, tc := range []struct {
		method, path, caller, body string
		status                     int
	}{
		{"PUT", "broadcast-range", "account-api", good, 403},
		{"PUT", "broadcast-range", "hhc-web-api", good, 200},
		{"PUT", "broadcast-range", "hhc-web-api", strings.Replace(good, `,"revoked":false`, "", 1), 400},
		{"PUT", "broadcast-range", "hhc-web-api", strings.Replace(good, `"epoch":1`, `"epoch":0`, 1), 400},
		{"PUT", "broadcast-range", "hhc-web-api", strings.Replace(good, `"revoked":false`, `"revoked":false,"extra":1`, 1), 400},
		{"PUT", "broadcast-range", "hhc-web-api", strings.Replace(good, `"revoked":false`, `"revoked":false,"memberState":"vod"`, 1), 200},
		{"PUT", "broadcast-range", "hhc-web-api", strings.Replace(good, `"revoked":false`, `"revoked":false,"memberState":null`, 1), 400},
		{"PUT", "broadcast-range", "hhc-web-api", strings.Replace(good, `"revoked":false`, `"revoked":false,"memberState":"invalid"`, 1), 400},
		{"GET", "broadcast-projection", "account-api", "", 403},
		{"GET", "broadcast-projection", "hhc-web-api", "", 200},
		{"POST", "preview-grant", "account-api", `{}`, 403},
	} {
		r := httptest.NewRequest(tc.method, "/priv/recording-captures/"+strings.Repeat("a", 32)+"/"+tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.path, tc.caller, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("broadcast response cacheable")
		}
	}
}
