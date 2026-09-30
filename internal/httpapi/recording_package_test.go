package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/storage/r2"
)

type httpPackageRepo struct {
	assets.RecordingPackageRepository
	p assets.RecordingPackage
}

func (s *httpPackageRepo) FindByIdempotency(context.Context, string) (assets.RecordingPackage, error) {
	return assets.RecordingPackage{}, assets.ErrNotFound
}
func (s *httpPackageRepo) Create(_ context.Context, p assets.RecordingPackage) error {
	s.p = p
	return nil
}
func (s *httpPackageRepo) Get(context.Context, string) (assets.RecordingPackage, error) {
	return s.p, nil
}
func (s *httpPackageRepo) Freeze(context.Context, string, time.Time) error {
	s.p.State = "freezing"
	return nil
}
func (s *httpPackageRepo) WithSessionLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

type httpPackageObjects struct{}

func (httpPackageObjects) Head(context.Context, string) (int64, string, error) {
	return 0, "", r2.ErrNotFound
}
func (httpPackageObjects) PresignPackageObject(context.Context, string, int64, string, time.Duration) (r2.PresignedPart, error) {
	return r2.PresignedPart{URL: "https://object.invalid", Method: "PUT"}, nil
}

func TestRecordingPackageRoutesFailClosed(t *testing.T) {
	handler := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, route := range []struct{ method, path string }{{"POST", "/priv/recording-packages"}, {"GET", "/priv/recording-packages/package-a"}, {"POST", "/priv/recording-packages/package-a/sign"}, {"POST", "/priv/recording-packages/package-a/complete"}} {
		for _, tc := range []struct {
			caller string
			status int
		}{{"", 403}, {"account-api", 403}, {"hhc-web-api", 503}} {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("%s caller=%q: %d %s", route.path, tc.caller, w.Code, w.Body.String())
			}
		}
	}
}

func TestRecordingPackageCreateAndCompleteHTTP(t *testing.T) {
	repo := &httpPackageRepo{}
	svc := assets.NewRecordingPackageService(repo, httpPackageObjects{}, time.Now)
	handler := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingPackages(svc).Routes()
	inv := assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1", Objects: []assets.RecordingPackageObject{
		{Path: "master.m3u8", SizeBytes: 100, SHA256: strings.Repeat("a", 64)}, {Path: "720p/index.m3u8", SizeBytes: 100, SHA256: strings.Repeat("b", 64)},
		{Path: "720p/init.mp4", SizeBytes: 100, SHA256: strings.Repeat("c", 64)}, {Path: "720p/seg-000000.m4s", SizeBytes: 100, SHA256: strings.Repeat("d", 64)},
	}, Renditions: []assets.RecordingRendition{{Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}}}
	inv.InventoryDigest, _ = assets.RecordingInventoryDigest(inv)
	body, _ := json.Marshal(assets.CreateRecordingPackageInput{RecordingID: "recording-a", Inventory: inv})
	request := func(method, path, body string, actor string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		r.Header.Set("X-HHC-Actor-ID", actor)
		r.Header.Set("Idempotency-Key", "operation-a")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request("POST", "/priv/recording-packages", string(body), "actor-a")
	if w.Code != 201 || repo.p.ActorID != "actor-a" || strings.Contains(w.Body.String(), "staging") || strings.Contains(w.Body.String(), "actorId") {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	path := "/priv/recording-packages/" + repo.p.ID
	w = request("POST", path+"/complete", "", "other-actor")
	if w.Code != 403 {
		t.Fatalf("cross-actor: %d", w.Code)
	}
	w = request("POST", path+"/sign", `{"paths":["master.m3u8"]}`, "actor-a")
	if w.Code != 200 {
		t.Fatalf("sign: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", path+"?limit=1001", "", "actor-a")
	if w.Code != 400 {
		t.Fatalf("page cap: %d", w.Code)
	}
	w = request("POST", path+"/complete", "", "actor-a")
	if w.Code != 202 || repo.p.State != "freezing" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	w = request("POST", path+"/sign", `{"paths":["master.m3u8"]}`, "actor-a")
	if w.Code != 409 {
		t.Fatalf("sign frozen: %d", w.Code)
	}
	w = request("POST", "/priv/recording-packages", strings.Replace(string(body), `"schemaVersion":1`, `"schemaVersion":1,"unknown":true`, 1), "actor-a")
	if w.Code != 400 {
		t.Fatalf("unknown inventory field: %d", w.Code)
	}
	large := strings.Replace(string(body), `"inventory":{`, `"inventory":{`+strings.Repeat(" ", 1<<20), 1)
	w = request("POST", "/priv/recording-packages", large, "actor-a")
	if w.Code != 201 {
		t.Fatalf("valid inventory over ordinary 1MiB body limit: %d", w.Code)
	}
	tooLarge := strings.Replace(string(body), `"inventory":{`, `"inventory":{`+strings.Repeat(" ", assets.RecordingInventoryMaxBytes), 1)
	w = request("POST", "/priv/recording-packages", tooLarge, "actor-a")
	if w.Code != 400 {
		t.Fatalf("oversized inventory: %d", w.Code)
	}
}
