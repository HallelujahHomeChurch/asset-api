package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type readyBatchRepo struct {
	assets.RecordingPackageRepository
	values map[string]assets.RecordingPackage
	err    error
	errAt  int
	calls  int
}

func (r *readyBatchRepo) Get(ctx context.Context, id string) (assets.RecordingPackage, error) {
	r.calls++
	if err := ctx.Err(); err != nil {
		return assets.RecordingPackage{}, err
	}
	if r.err != nil && (r.errAt == 0 || r.calls == r.errAt) {
		return assets.RecordingPackage{}, r.err
	}
	p, ok := r.values[id]
	if !ok {
		return p, assets.ErrNotFound
	}
	return p, nil
}

func readyBatchRequest(h http.Handler, body, caller string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/priv/recording-packages/ready-batch", strings.NewReader(body))
	r.Header.Set("X-Internal-Caller-App-Id", caller)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReadyBatchMatchesCanonicalReadyAndKeepsPrivateFieldsOut(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	ready, expiry := now.Add(-time.Hour), now.Add(time.Hour)
	for _, count := range []int{1, 25, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			repo := &readyBatchRepo{values: map[string]assets.RecordingPackage{}}
			var items []assets.RecordingLifecycleBinding
			for i := range count {
				id := fmt.Sprintf("package-%d", i)
				recording := fmt.Sprintf("recording-%d", i)
				items = append(items, assets.RecordingLifecycleBinding{PackageID: id, RecordingID: recording})
				repo.values[id] = assets.RecordingPackage{ID: id, SessionID: id, RecordingID: recording, OwnerService: "hhc-web-api", ActorID: "private-uploader", State: "ready", ReadyAt: &ready, UploadedAt: &ready, MediaExpiresAt: &expiry, RetentionRevision: 3, FinalPrefix: "private/final/", Inventory: assets.RecordingPackageInventory{Objects: []assets.RecordingPackageObject{{Path: "private-object"}}, Renditions: []assets.RecordingRendition{{Name: "720p", DurationSeconds: 42}}}}
			}
			svc := assets.NewRecordingPackageService(repo, httpPackageObjects{}, func() time.Time { return now })
			h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", CallerConfig{}, nil).WithRecordingPackages(svc).Routes()
			body, _ := json.Marshal(map[string]any{"items": items})
			w := readyBatchRequest(h, string(body), "hhc-web-api")
			if w.Code != 200 || repo.calls != count || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, repo.calls, w.Body.String())
			}
			var response struct {
				Items []struct {
					assets.RecordingLifecycleBinding
					Status  string                        `json:"status"`
					Package assets.RecordingPackageStatus `json:"package"`
				} `json:"items"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Items) != count {
				t.Fatalf("invalid response: %v", err)
			}
			for i, item := range response.Items {
				if item.RecordingLifecycleBinding != items[i] || item.Status != "ready" || item.Package.SessionID != items[i].PackageID || item.Package.UploadedAt == nil || item.Package.RetentionRevision != 3 || item.Package.Renditions[0].DurationSeconds != 42 {
					t.Fatalf("wrong ready projection: %+v", item)
				}
			}
			for _, private := range []string{"private-uploader", "private/final", "private-object", "inventory", "ownerService"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatalf("private field exposed: %s", private)
				}
			}
		})
	}
}

func TestReadyBatchValidatesBeforeAnyLookupAndFailsClosed(t *testing.T) {
	repo := &readyBatchRepo{}
	svc := assets.NewRecordingPackageService(repo, httpPackageObjects{}, time.Now)
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", CallerConfig{}, nil).WithRecordingPackages(svc).Routes()
	valid := `{"items":[{"recordingId":"r","packageId":"p"}]}`
	var oversized []assets.RecordingLifecycleBinding
	for i := range 101 {
		oversized = append(oversized, assets.RecordingLifecycleBinding{RecordingID: "r", PackageID: fmt.Sprint(i)})
	}
	large, _ := json.Marshal(map[string]any{"items": oversized})
	for _, body := range []string{`{}`, `{"items":[]}`, `{"items":[{"recordingId":"","packageId":"p"}]}`, `{"items":[{"recordingId":"r","packageId":"p"},{"recordingId":"r","packageId":"p"}]}`, `{"items":[{"recordingId":"r","packageId":"p","unknown":true}]}`, valid + `{}`, string(large), `{"items":[{"recordingId":"` + strings.Repeat("x", 33_000) + `","packageId":"p"}]}`} {
		w := readyBatchRequest(h, body, "hhc-web-api")
		if w.Code != 400 || repo.calls != 0 {
			t.Fatalf("validation status=%d lookups=%d", w.Code, repo.calls)
		}
	}
	for _, caller := range []string{"", "account-api"} {
		if w := readyBatchRequest(h, valid, caller); w.Code != 403 || repo.calls != 0 {
			t.Fatalf("caller admitted: %q %d", caller, w.Code)
		}
	}
	for _, err := range []error{errors.New("private store failure"), context.Canceled} {
		repo.err = err
		w := readyBatchRequest(h, valid, "hhc-web-api")
		if w.Code < 500 || strings.Contains(w.Body.String(), "private store failure") {
			t.Fatalf("store error swallowed/exposed: %d %s", w.Code, w.Body.String())
		}
	}
	repo.calls, repo.errAt = 0, 2
	now := time.Now()
	ready, expiry := now.Add(-time.Hour), now.Add(time.Hour)
	repo.values = map[string]assets.RecordingPackage{"p": {ID: "p", OwnerService: "hhc-web-api", RecordingID: "r", State: "ready", ReadyAt: &ready, MediaExpiresAt: &expiry, FinalPrefix: "final/"}}
	w := readyBatchRequest(h, `{"items":[{"recordingId":"r","packageId":"p"},{"recordingId":"r","packageId":"q"}]}`, "hhc-web-api")
	if w.Code < 500 || strings.Contains(w.Body.String(), `"items"`) || repo.calls != 2 {
		t.Fatalf("partial success leaked: %d %s", w.Code, w.Body.String())
	}
	repo.calls = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/priv/recording-packages/ready-batch", strings.NewReader(valid)).WithContext(ctx)
	r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code < 500 || repo.calls != 0 {
		t.Fatalf("canceled batch continued: %d calls=%d", w.Code, repo.calls)
	}
	noService := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", CallerConfig{}, nil).Routes()
	if w := readyBatchRequest(noService, valid, "hhc-web-api"); w.Code != 503 {
		t.Fatalf("disabled route: %d", w.Code)
	}
}

func TestReadyBatchPreservesCanonicalPerItemDenialsAndCompositeBindings(t *testing.T) {
	now := time.Now().UTC()
	ready, expiry := now.Add(-time.Hour), now.Add(time.Hour)
	base := assets.RecordingPackage{ID: "p", SessionID: "p", RecordingID: "r", OwnerService: "hhc-web-api", State: "ready", ReadyAt: &ready, MediaExpiresAt: &expiry, FinalPrefix: "final/"}
	for _, test := range []struct {
		name   string
		mutate func(*assets.RecordingPackage)
	}{
		{"owner", func(p *assets.RecordingPackage) { p.OwnerService = "account-api" }},
		{"binding", func(p *assets.RecordingPackage) { p.RecordingID = "other" }},
		{"state", func(p *assets.RecordingPackage) { p.State = "freezing" }},
		{"readyAt", func(p *assets.RecordingPackage) { p.ReadyAt = nil }},
		{"finalPrefix", func(p *assets.RecordingPackage) { p.FinalPrefix = "" }},
		{"expiry", func(p *assets.RecordingPackage) { p.MediaExpiresAt = &now }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := base
			test.mutate(&p)
			repo := &readyBatchRepo{values: map[string]assets.RecordingPackage{"p": p}}
			h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", CallerConfig{}, nil).WithRecordingPackages(assets.NewRecordingPackageService(repo, httpPackageObjects{}, func() time.Time { return now })).Routes()
			w := readyBatchRequest(h, `{"items":[{"recordingId":"r","packageId":"p"},{"recordingId":"other","packageId":"p"},{"recordingId":"r","packageId":"missing"}]}`, "hhc-web-api")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"forbidden"`) || !strings.Contains(w.Body.String(), `"status":"not_found"`) || repo.calls != 3 {
				t.Fatalf("denials: %d %s calls=%d", w.Code, w.Body.String(), repo.calls)
			}
		})
	}
}

func TestReadyBatchUsesProductionInternalAdmission(t *testing.T) {
	repo := &readyBatchRepo{}
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, false, "test-app-channel-token", CallerConfig{}, nil).WithRecordingPackages(assets.NewRecordingPackageService(repo, httpPackageObjects{}, time.Now)).Routes()
	for _, tc := range []struct {
		headers map[string]string
		status  int
	}{
		{map[string]string{"X-HHC-Caller-App-Id": "hhc-web-api"}, 200},
		{map[string]string{"Dapr-Caller-App-Id": "hhc-web-api", "dapr-api-token": "test-app-channel-token"}, 200},
		{map[string]string{"Dapr-Caller-App-Id": "hhc-web-api"}, 403},
		{map[string]string{"Dapr-Caller-App-Id": "hhc-web-api", "dapr-api-token": "wrong"}, 403},
		{map[string]string{"X-Internal-Caller-App-Id": "hhc-web-api"}, 403},
		{map[string]string{"X-MS-CLIENT-PRINCIPAL": "untrusted"}, 403},
		{map[string]string{"X-HHC-Caller-App-Id": "hhc-web-api", "Dapr-Caller-App-Id": "hhc-web-api"}, 403},
		{map[string]string{"X-HHC-Caller-App-Id": "account-api"}, 403},
	} {
		r := httptest.NewRequest("POST", "/priv/recording-packages/ready-batch", strings.NewReader(`{"items":[{"recordingId":"r","packageId":"p"}]}`))
		for name, value := range tc.headers {
			r.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("admission status=%d want=%d", w.Code, tc.status)
		}
	}
}
