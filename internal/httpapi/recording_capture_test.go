package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCaptureAdmissionLogsAcceptedSequenceAndTimingWithoutPrivateInputs(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	now := time.Now().UTC()
	id := strings.Repeat("a", 32)
	actor := "22222222-2222-4222-8222-222222222222"
	repo := &httpCaptureRepo{c: assets.RecordingCapture{ID: id, ActorID: actor, State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}}
	h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCaptures(assets.NewRecordingCaptureService(repo, admissionObjects{}, time.Now)).Routes()
	for _, request := range []struct {
		route, body string
		status      int
	}{
		{"objects", `{"operationKey":"private-operation-key","objects":[{"path":"720p/seg-000044.m4s","sizeBytes":10,"sha256":"` + strings.Repeat("b", 64) + `"}]}`, 200},
		{"confirm", `{"operationKey":"private-confirm-key","paths":["720p/seg-000044.m4s"]}`, 202},
	} {
		r := httptest.NewRequest("POST", "/priv/recording-captures/"+id+"/"+request.route, strings.NewReader(request.body))
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		r.Header.Set("X-HHC-Actor-ID", actor)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != request.status {
			t.Fatalf("admission: %d %s", w.Code, w.Body.String())
		}
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var value map[string]any
		if json.Unmarshal([]byte(line), &value) == nil && value["msg"] == "recording_capture_admission" {
			events = append(events, value)
		}
	}
	if len(events) != 2 {
		t.Fatalf("accepted declare/confirm not traceable: %s", logs.String())
	}
	for index, event := range events {
		operation := []string{"declare", "confirm"}[index]
		if event["capture_id"] != id || event["operation"] != operation || event["first_sequence"] != float64(44) || event["last_sequence"] != float64(44) || event["objects"] != float64(1) || event["accepted_at"] == nil || event["elapsed_ms"] == nil {
			t.Fatalf("admission evidence: %+v", event)
		}
	}
	for _, private := range []string{actor, "private-operation-key", "private-confirm-key", strings.Repeat("b", 64), "720p/seg-000044.m4s", "https://"} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("private input logged: %s", private)
		}
	}
}

type admissionObjects struct{ httpPackageObjects }

func (admissionObjects) ListPackageObjects(context.Context, string, int) (map[string]int64, error) {
	return map[string]int64{"720p/seg-000044.m4s": 10}, nil
}

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

type failingCaptureObjects struct{ httpPackageObjects }

func (failingCaptureObjects) ListPackageObjects(context.Context, string, int) (map[string]int64, error) {
	return nil, errors.New("provider unavailable")
}

type failingCaptureRepository struct {
	assets.RecordingCaptureRepository
}

func (failingCaptureRepository) GetCapture(context.Context, string) (assets.RecordingCapture, error) {
	return assets.RecordingCapture{}, errors.New("database unavailable")
}
func TestCaptureDependencyFailuresReturnUnavailable503(t *testing.T) {
	actor := "22222222-2222-4222-8222-222222222222"
	repo := &httpCaptureRepo{c: assets.RecordingCapture{ID: "capture-a", ActorID: actor, State: "uploading", ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}, Objects: []assets.RecordingCaptureObject{{RecordingPackageObject: assets.RecordingPackageObject{Path: "720p/init.mp4", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}, State: "declared"}}}}
	for _, tc := range []struct {
		name, method, path, body string
		service                  *assets.RecordingCaptureService
	}{
		{"provider", "POST", "/priv/recording-captures/capture-a/confirm", `{"operationKey":"confirm-a","paths":["720p/init.mp4"]}`, assets.NewRecordingCaptureService(repo, failingCaptureObjects{}, time.Now)},
		{"database", "GET", "/priv/recording-captures/capture-a", "", assets.NewRecordingCaptureService(failingCaptureRepository{}, httpPackageObjects{}, time.Now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCaptures(tc.service).Routes()
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
			r.Header.Set("X-HHC-Actor-ID", actor)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"capture_unavailable"`) {
				t.Fatalf("dependency error: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCaptureLiveProgressRequiresUploaderAndReportsPublishedWaterline(t *testing.T) {
	actor := "22222222-2222-4222-8222-222222222222"
	repo := &httpCaptureRepo{c: assets.RecordingCapture{ID: strings.Repeat("a", 32), ActorID: actor, Progress: assets.RecordingLiveProgress{Revision: 3, LastSequence: 2, MediaEndSeconds: 90}}}
	svc := assets.NewRecordingCaptureService(repo, httpPackageObjects{}, time.Now)
	h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCaptures(svc).Routes()
	for _, owner := range []string{"", "other", actor} {
		r := httptest.NewRequest("GET", "/priv/recording-captures/"+repo.c.ID+"/progress", nil)
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		r.Header.Set("X-HHC-Actor-ID", owner)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 403
		if owner == actor {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("progress auth: %d %s", w.Code, w.Body.String())
		}
		if want == 200 && (!strings.Contains(w.Body.String(), `"lastSequence":2`) || w.Header().Get("Cache-Control") != "private, no-store") {
			t.Fatal(w.Body.String())
		}
	}
}
