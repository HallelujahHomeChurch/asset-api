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
		{"AST_SOURCE_CLIENT_UPGRADE", "collection", 426}, {"AST_SOURCE_PAUSED", "collection", 423}, {"AST_SOURCE_EXPIRED", "item", 410}, {"AST_SOURCE_REVOKED", "collection", 410}, {"AST_SOURCE_UNAVAILABLE", "dependency", 503},
	} {
		w := httptest.NewRecorder()
		handleError(w, &assets.SourceAccessError{Code: tc.code, Scope: tc.scope})
		var body struct {
			Error struct {
				Code, Scope string
				Version     int
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != tc.status || body.Error.Version != 1 || body.Error.Code != tc.code || body.Error.Scope != tc.scope || w.Header().Get("Cache-Control") != "private, no-store" {
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
	return assets.SourceDecision{Version: 1, RevocationRevision: 1, State: a.state, PolicyRevision: 1}, nil
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
			if state == "active" && w.Header().Get("X-HHC-Source-Expires-At") != a.Source.ExpiresAt.UTC().Format(time.RFC3339Nano) {
				t.Fatal("source deadline missing from content response")
			}
			if w.Code != want || authority.calls != beforeCalls+1 || (state != "active" && blobs.openCalls != beforeOpen) {
				t.Fatalf("%s %s: status=%d calls=%d opens=%d body=%s", tc.method, state, w.Code, authority.calls-beforeCalls, blobs.openCalls-beforeOpen, w.Body)
			}
		}
	}
}

type recoveryUploadRepository struct {
	assets.Repository
	asset   assets.Asset
	session assets.UploadSession
}

func (r *recoveryUploadRepository) FindUploadByIdempotency(_ context.Context, caller, operation, key string) (assets.Asset, assets.UploadSession, error) {
	if caller != assets.LineHarnessCaller || operation != "create_upload" || key != "recovery_operation_123" {
		return assets.Asset{}, assets.UploadSession{}, assets.ErrNotFound
	}
	return r.asset, r.session, nil
}
func TestSourceUploadRecoveryRequiresDedicatedPrincipalAndReturnsNoTarget(t *testing.T) {
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers[assets.LineHarnessCaller] = true
	h.workloadAuth.config.Principals["harness"] = assets.LineHarnessCaller
	now := time.Now().UTC()
	a := assets.Asset{ID: "original", OwnerService: assets.LineHarnessCaller, Namespace: "line.group.file", UploadProfiles: assets.UploadProfiles{SecurityProfile: assets.LineSourceSecurityProfile, ProcessingProfile: "no-processing-v1"}, Source: &assets.SourcePolicy{Version: 1, SourceReference: assets.SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}, ExpiresAt: now.Add(assets.SourceLifetime)}}
	h.service = assets.NewService(&recoveryUploadRepository{asset: a, session: assets.UploadSession{ID: "session", AssetID: a.ID, CallerService: assets.LineHarnessCaller}}, nil, "", func() time.Time { return now })
	for _, principal := range []string{"harness", "cms", ""} {
		r := httptest.NewRequest("GET", "/priv/assets/upload-sessions/recovery_operation_123", nil)
		if principal != "" {
			r.Header.Set(workloadHeader, f.bearer(t, "first", f.firstKey, nil, f.claims(principal)))
		}
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, r)
		if principal == "harness" {
			var result assets.CreatedUpload
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Asset.ID != a.ID || result.Target.URL != "" {
				t.Fatalf("recovery status=%d body=%s", w.Code, w.Body)
			}
		} else if w.Code < 400 {
			t.Fatalf("principal %s allowed", principal)
		}
	}
}

func TestSourceProtocolHeaderIsSingleExactCapability(t *testing.T) {
	for _, values := range [][]string{nil, {"1"}, {"2"}, {"1", "1"}, {"1, 1"}, {"01"}} {
		r := httptest.NewRequest("GET", "/api/assets/collections", nil)
		r.Header.Set("X-HHC-User-ID", "reader")
		r.Header.Set("X-HHC-Token-Expires-At", "2000000000")
		for _, v := range values {
			r.Header.Add("X-HHC-Source-Policy-Version", v)
		}
		identity, ok := parseCollectionReaderIdentity(r)
		want := 0
		if len(values) == 1 && values[0] == "1" {
			want = 1
		}
		if !ok || identity.Subject.SourcePolicyVersion != want {
			t.Fatalf("%v: %+v", values, identity)
		}
	}
}

func (a *httpSourceAuthority) CheckSources(ctx context.Context, requests []assets.SourceAccessRequest) ([]assets.SourceDecision, error) {
	result := make([]assets.SourceDecision, 0, len(requests))
	for _, r := range requests {
		d, err := a.CheckSource(ctx, r)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}
