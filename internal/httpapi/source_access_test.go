package httpapi

import (
	"context"
	"encoding/json"
	"hhc/asset-api/internal/assets"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSourceErrorsPreserveScopeAndRecoverability(t *testing.T) {
	for _, tc := range []struct {
		code, scope string
		status      int
	}{
		{"AST_SOURCE_PAUSED", "collection", 423}, {"AST_SOURCE_EXPIRED", "item", 410}, {"AST_SOURCE_REVOKED", "collection", 410}, {"AST_SOURCE_UNAVAILABLE", "dependency", 503},
	} {
		w := httptest.NewRecorder()
		handleError(w, &assets.SourceAccessError{Code: tc.code, Scope: tc.scope})
		var body struct{ Error struct{ Code, Scope string } }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != tc.status || body.Error.Code != tc.code || body.Error.Scope != tc.scope || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
}

type httpSourceAuthority struct {
	state string
	calls int
}

func (a *httpSourceAuthority) CheckSource(_ context.Context, _ assets.SourceAccessRequest) (assets.SourceDecision, error) {
	a.calls++
	return assets.SourceDecision{Version: 1, State: a.state, PolicyRevision: 1}, nil
}

func TestSourceContentChecksAuthorityBeforeGETHEADRangeAndConditionalResponse(t *testing.T) {
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers[assets.LineHarnessCaller] = true
	h.workloadAuth.config.Principals["harness"] = assets.LineHarnessCaller
	now := time.Now().UTC()
	payload := []byte("source bytes")
	a := assets.Asset{ID: "source", Namespace: "line.group.file", OwnerService: assets.LineHarnessCaller, UploadProfiles: assets.UploadProfiles{SecurityProfile: assets.LineSourceSecurityProfile, ProcessingProfile: "no-processing-v1"}, Source: &assets.SourcePolicy{SourceReference: assets.SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}, Version: 1, ExpiresAt: now.Add(assets.SourceLifetime)}, UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanNotRequired, ProcessingStatus: assets.ProcessingNotRequired, ETag: `"etag"`, ObjectKey: "private", SizeBytes: int64(len(payload)), DetectedMIMEType: "application/pdf", OriginalFileName: "file.pdf"}
	blobs := &downloadBlobStore{objects: map[string][]byte{"private": payload}}
	authority := &httpSourceAuthority{state: "active"}
	h.service = assets.NewService(&completedDerivationRepository{asset: a}, blobs, "", func() time.Time { return now }).WithSourceAuthority(authority, nil)
	for _, tc := range []struct {
		method, rangeValue, conditional string
		want                            int
	}{{"GET", "", "", 200}, {"HEAD", "", "", 200}, {"GET", "bytes=0-3", "", 206}, {"GET", "", `"etag"`, 304}} {
		for _, state := range []string{"active", "paused", "revoked", "unknown"} {
			authority.state = state
			beforeCalls, beforeOpen := authority.calls, blobs.openCalls
			r := httptest.NewRequest(tc.method, "/priv/assets/source/source-content", nil)
			r.Header.Set(workloadHeader, f.bearer(t, "first", f.firstKey, nil, f.claims("harness")))
			r.Header.Set("Range", tc.rangeValue)
			r.Header.Set("If-None-Match", tc.conditional)
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			want := tc.want
			if state == "paused" {
				want = 423
			}
			if state == "revoked" {
				want = 410
			}
			if state == "unknown" {
				want = 503
			}
			if w.Code != want || authority.calls != beforeCalls+1 || (state != "active" && blobs.openCalls != beforeOpen) {
				t.Fatalf("%s %s: status=%d calls=%d opens=%d body=%s", tc.method, state, w.Code, authority.calls-beforeCalls, blobs.openCalls-beforeOpen, w.Body)
			}
		}
	}
}
