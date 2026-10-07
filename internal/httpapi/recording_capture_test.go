package httpapi

import (
	"context"
	"encoding/json"
	"hhc/asset-api/internal/assets"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCaptureRoutesFailClosed(t *testing.T) {
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, route := range []string{"", "/capture-a", "/capture-a/objects", "/capture-a/sign", "/capture-a/confirm", "/capture-a/seal", "/capture-a/abort", "/capture-a/progress", "/capture-a/grant"} {
		for _, caller := range []string{"hhc-web-api", "account-api"} {
			method := "POST"
			if route == "/capture-a" || route == "/capture-a/progress" {
				method = "GET"
			}
			r := httptest.NewRequest(method, "/priv/recording-captures"+route, strings.NewReader(`{}`))
			r.Header.Set("X-Internal-Caller-App-Id", caller)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 503
			if caller != "hhc-web-api" {
				want = 403
			}
			if w.Code != want || !strings.Contains(w.Body.String(), "capture_") {
				t.Fatalf("%s %s: %d %s", route, caller, w.Code, w.Body.String())
			}
		}
	}
}

type httpCaptureRepo struct{ c assets.RecordingCapture }

func (r *httpCaptureRepo) CreateCapture(_ context.Context, c assets.RecordingCapture) (assets.RecordingCapture, error) {
	r.c = c
	return c, nil
}
func (r *httpCaptureRepo) GetCapture(_ context.Context, id string) (assets.RecordingCapture, error) {
	if id != r.c.ID {
		return assets.RecordingCapture{}, assets.ErrNotFound
	}
	return r.c, nil
}
func (r *httpCaptureRepo) UpdateCapture(_ context.Context, id string, fn func(*assets.RecordingCapture) error) (assets.RecordingCapture, error) {
	if id != r.c.ID {
		return assets.RecordingCapture{}, assets.ErrNotFound
	}
	err := fn(&r.c)
	return r.c, err
}
func TestCaptureHTTPReceiptsAndInvalidBodyContract(t *testing.T) {
	repo := &httpCaptureRepo{}
	svc := assets.NewRecordingCaptureService(repo, httpPackageObjects{}, time.Now)
	h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCaptures(svc).Routes()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		r.Header.Set("X-HHC-Actor-ID", "22222222-2222-4222-8222-222222222222")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("POST", "/priv/recording-captures", `{"recordingId":"11111111-1111-4111-8111-111111111111","operationKey":"create-a"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var result assets.RecordingCaptureResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Receipt.Operation != "create" || result.Capture.Progress.LastSequence != -1 || result.Capture.PackageID != nil {
		t.Fatalf("result: %+v %v", result, err)
	}
	w = request("POST", "/priv/recording-captures", `{"unknown":1}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"capture_invalid"`) {
		t.Fatalf("invalid contract: %d %s", w.Code, w.Body.String())
	}
	w = request("POST", "/priv/recording-captures/"+result.Capture.ID+"/abort", `{"operationKey":"abort-a","reasonCode":"user_abort"}`)
	if w.Code != 200 {
		t.Fatalf("abort: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/priv/recording-captures/"+result.Capture.ID+"?limit=1001", "")
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
