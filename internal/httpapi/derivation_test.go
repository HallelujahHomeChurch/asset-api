package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestStructuredDerivationAdmissionDefaultsOff(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	h.workloadAuth.config.TrustedDerivationPrincipals = []string{"watermark"}
	proof := assets.Derivation{SourceAssetID: "synthetic-source", SourceSHA256: strings.Repeat("c", 64), SourceETag: "source-version", Transform: "bulletin-watermark", TransformVersion: "1"}
	asset := assets.Asset{ID: "output", OwnerService: "hhc-web-api", UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean, ScanSignature: assets.TrustedBulletinWatermark, ProcessingStatus: assets.ProcessingNotRequired, SizeBytes: 32, ChecksumSHA256: strings.Repeat("d", 64), DetectedMIMEType: "application/pdf", ETag: "output-etag", Derivation: proof}
	h.service = assets.NewService(&completedDerivationRepository{asset: asset}, completedDerivationBlobStore{}, "", time.Now)
	body, err := json.Marshal(struct {
		assets.CompleteUploadInput
		Derivation assets.Derivation `json:"derivation"`
	}{assets.CompleteUploadInput{SizeBytes: 32, ChecksumSHA256: asset.ChecksumSHA256, MIMEType: "application/pdf"}, proof})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/output/complete", strings.NewReader(string(body)))
	request.Header.Set(workloadHeader, fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims("watermark")))
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("default-off proof admission status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGenericDerivationScopeAndVerifiedReadiness(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	h.workloadAuth.config.DisableLegacyCallers = nil
	outputHash := strings.Repeat("d", 64)
	proof := assets.Derivation{SourceAssetID: "synthetic-source", SourceSHA256: strings.Repeat("c", 64), SourceETag: "source-version", Transform: "bulletin-watermark", TransformVersion: "1"}
	body := func(raw string) string {
		return `{"sizeBytes":32,"checksumSha256":"` + outputHash + `","mimeType":"application/pdf"` + raw + "}"
	}
	proofJSON, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	derived := body(`,"derivation":` + string(proofJSON))
	for _, test := range []struct {
		name, method, principal, body, wantState, wantKind string
		disabled                                           bool
		change                                             func(*assets.Asset)
		want                                               int
	}{
		{name: "authorized replay", principal: "watermark", body: derived, want: 200, wantState: "ready", wantKind: "trusted-derived"},
		{name: "different proof", principal: "watermark", body: strings.Replace(derived, "source-version", "another-version", 1), want: 409},
		{name: "transform denied", principal: "watermark", body: strings.Replace(derived, "bulletin-watermark", "unapproved", 1), want: 400},
		{name: "scope unset", principal: "watermark", body: derived, disabled: true, want: 403},
		{name: "verified CMS denied", principal: "cms", body: derived, want: 403},
		{name: "legacy worker denied", principal: "legacy-watermark", body: derived, want: 403},
		{name: "explicit null rejected", principal: "watermark", body: body(`,"derivation":null`), want: 400},
		{name: "unknown nested member rejected", principal: "watermark", body: strings.Replace(derived, `"sourceAssetId"`, `"skipScan":true,"sourceAssetId"`, 1), want: 400},
		{name: "ordinary winner cannot promote", principal: "watermark", body: derived, change: func(a *assets.Asset) {
			a.ScanStatus, a.ScanSignature, a.Derivation = assets.ScanPending, "", assets.Derivation{}
		}, want: 409},
		{name: "legacy GET stable", method: http.MethodGet, principal: "legacy-cms", want: 200},
		{name: "legacy complete stable", principal: "legacy-cms", body: body(""), want: 200},
		{name: "ordinary identical replay retains evidence", principal: "cms", body: body(""), want: 200, wantState: "ready", wantKind: "trusted-derived"},
		{name: "verified GET ready scanned", method: http.MethodGet, principal: "cms", change: func(a *assets.Asset) {
			a.ScanSignature, a.Derivation = "", assets.Derivation{}
		}, want: 200, wantState: "ready", wantKind: "scanned"},
		{name: "verified GET pending", method: http.MethodGet, principal: "cms", change: func(a *assets.Asset) {
			a.ScanStatus, a.ScanSignature, a.Derivation = assets.ScanPending, "", assets.Derivation{}
		}, want: 200, wantState: "pending"},
		{name: "verified GET infected", method: http.MethodGet, principal: "cms", change: func(a *assets.Asset) {
			a.ScanStatus = assets.ScanInfected
		}, want: 200, wantState: "rejected"},
		{name: "verified GET unknown processing fails closed", method: http.MethodGet, principal: "cms", change: func(a *assets.Asset) {
			a.ProcessingStatus = "future"
		}, want: 200, wantState: "rejected", wantKind: "trusted-derived"},
	} {
		t.Run(test.name, func(t *testing.T) {
			asset := assets.Asset{
				ID: "output", OwnerService: "hhc-web-api", UploadStatus: assets.UploadCompleted,
				ScanStatus: assets.ScanClean, ScanSignature: assets.TrustedBulletinWatermark,
				ProcessingStatus: assets.ProcessingNotRequired, SizeBytes: 32, ChecksumSHA256: outputHash,
				DetectedMIMEType: "application/pdf", ETag: "output-etag", Derivation: proof,
			}
			if test.change != nil {
				test.change(&asset)
			}
			repo := &completedDerivationRepository{asset: asset}
			h.service = assets.NewService(repo, completedDerivationBlobStore{}, "", time.Now).WithDerivationProofEnabled(true)
			h.workloadAuth.config.TrustedDerivationPrincipals = nil
			if !test.disabled {
				h.workloadAuth.config.TrustedDerivationPrincipals = []string{"watermark"}
			}
			path, method := "/priv/assets/output", test.method
			if method == "" {
				path, method = path+"/complete", http.MethodPost
			}
			request := httptest.NewRequest(method, path, strings.NewReader(test.body))
			if strings.HasPrefix(test.principal, "legacy-") {
				caller := "hhc-web-api"
				if test.principal == "legacy-watermark" {
					caller = "hhc-web-bulletin-worker"
				}
				request.Header.Set("X-HHC-Caller-App-Id", caller)
			} else {
				request.Header.Set(workloadHeader, fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims(test.principal)))
			}
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, request)
			if response.Code != test.want || strings.Contains(response.Body.String(), proof.SourceAssetID) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Code != http.StatusOK {
				return
			}
			if test.wantState == "" {
				legacy, _ := json.Marshal(asset)
				if response.Body.String() != string(legacy)+"\n" {
					t.Fatalf("legacy projection changed: %s", response.Body.String())
				}
				return
			}
			var result struct {
				ScanStatus          assets.ScanStatus `json:"scanStatus"`
				ProcessingReadiness struct {
					Version int    `json:"version"`
					State   string `json:"state"`
				} `json:"processingReadiness"`
				SafetyEvidence struct {
					Kind string `json:"kind"`
				} `json:"safetyEvidence"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.ScanStatus != asset.ScanStatus ||
				result.ProcessingReadiness.Version != 1 || result.ProcessingReadiness.State != test.wantState ||
				result.SafetyEvidence.Kind != test.wantKind {
				t.Fatalf("readiness projection=%+v body=%s err=%v", result, response.Body.String(), err)
			}
		})
	}
}

type completedDerivationRepository struct {
	assets.Repository
	asset assets.Asset
}

func (r *completedDerivationRepository) GetAsset(context.Context, string) (assets.Asset, error) {
	return r.asset, nil
}

func (*completedDerivationRepository) GetUploadSession(context.Context, string) (assets.UploadSession, error) {
	return assets.UploadSession{Status: assets.UploadCompleted}, nil
}

type completedDerivationBlobStore struct{ assets.BlobStore }

func (completedDerivationBlobStore) Delete(context.Context, string) error { return nil }
