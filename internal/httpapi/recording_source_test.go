package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestRecordingSourceRoutesFailClosed(t *testing.T) {
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, route := range []struct{ method, path string }{{"POST", "/priv/recording-sources"}, {"GET", "/priv/recording-sources/source-a"}, {"POST", "/priv/recording-sources/source-a/sign"}, {"POST", "/priv/recording-sources/source-a/complete"}} {
		for _, tc := range []struct {
			caller string
			want   int
		}{{"", 403}, {"account-api", 403}, {"hhc-web-api", 503}} {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("%s %s: %d %s", route.path, tc.caller, w.Code, w.Body.String())
			}
		}
	}
}

type httpSourceRepo struct {
	assets.RecordingSourceRepository
	p assets.RecordingSource
}

func (s *httpSourceRepo) WithSessionLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (s *httpSourceRepo) Get(context.Context, string) (assets.RecordingSource, error) {
	return s.p, nil
}
func (s *httpSourceRepo) Finalize(_ context.Context, _ string, etag string, at time.Time) error {
	s.p.State = "finalizing"
	s.p.StagingETag = etag
	s.p.CompletedAt = &at
	until := at.Add(assets.RecordingSourceRetention)
	s.p.RetryUntil = &until
	return nil
}

type httpSourceObjects struct{}

func (httpSourceObjects) CommitRecordingSource(context.Context, string, int64) (assets.BlobMetadata, error) {
	return assets.BlobMetadata{Size: 5, ETag: "private-etag"}, nil
}
func (httpSourceObjects) SignRecordingSourceBlock(_ context.Context, _ string, _ int, at time.Time) (assets.UploadTarget, error) {
	return assets.UploadTarget{URL: "https://blob.invalid?sig=private", Method: "PUT", ExpiresAt: at}, nil
}
func (httpSourceObjects) RecordingSourceBlocks(context.Context, string) (assets.RecordingSourceBlockList, error) {
	return assets.RecordingSourceBlockList{}, assets.ErrNotFound
}

func TestRecordingSourceCompleteAndWriteCapabilityHTTP(t *testing.T) {
	now := time.Now().UTC()
	repo := &httpSourceRepo{p: assets.RecordingSource{ID: strings.Repeat("a", 32), OwnerService: "hhc-web-api", ActorID: "actor-a", RecordingID: "recording-a", FileName: "source.mp4", SizeBytes: 5, BlockCount: 1, State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingSources(assets.NewRecordingSourceService(repo, httpSourceObjects{}, time.Now)).Routes()
	request := func(suffix, actor, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/priv/recording-sources/"+repo.p.ID+suffix, strings.NewReader(body))
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		r.Header.Set("X-HHC-Actor-ID", actor)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("/complete", "other", ""); w.Code != 403 {
		t.Fatalf("cross-owner %d", w.Code)
	}
	w := request("/sign", "actor-a", `{"numbers":[1]}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("capability: %d %s", w.Code, w.Body.String())
	}
	w = request("/complete", "actor-a", "")
	if w.Code != 202 || strings.Contains(w.Body.String(), "private-etag") || strings.Contains(w.Body.String(), "ownerService") {
		t.Fatalf("receipt %d %s", w.Code, w.Body.String())
	}
	var p assets.RecordingSource
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.State != "finalizing" || p.RetryUntil == nil {
		t.Fatalf("receipt %+v %v", p, err)
	}
	if w := request("/sign", "actor-a", `{"numbers":[1]}`); w.Code != 409 {
		t.Fatalf("signed completed source %d", w.Code)
	}
}
