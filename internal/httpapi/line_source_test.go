package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestLineSourcePolicyRequiresNewVerifiedProducer(t *testing.T) {
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers[assets.LineHarnessCaller] = true
	h.workloadAuth.config.Principals["harness"] = assets.LineHarnessCaller
	h.workloadAuth.config.UploadProfileScopes = map[string][]assets.UploadProfileScope{"harness": {{NamespaceFamily: "line.group.file", UploadProfiles: assets.UploadProfiles{SecurityProfile: assets.LineSourceSecurityProfile, ProcessingProfile: "no-processing-v1"}}}}
	for _, principal := range []string{"harness", "cms", ""} {
		r := httptest.NewRequest("POST", "/priv/assets/upload-policy", strings.NewReader(`{"namespace":"line.group.file","securityProfile":"line-source-unscanned-v1","processingProfile":"no-processing-v1"}`))
		if principal != "" {
			r.Header.Set(workloadHeader, f.bearer(t, "first", f.firstKey, nil, f.claims(principal)))
		} else {
			r.Header.Set("X-HHC-Caller-App-Id", assets.LineHarnessCaller)
		}
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, r)
		if principal != "harness" {
			if w.Code != 403 {
				t.Fatalf("unverified %s: %d", principal, w.Code)
			}
			continue
		}
		var body struct {
			Version                     int
			ScanRequired                bool
			ReadinessVersion            int
			SourceUploadRecoveryVersion int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.Version != 2 || body.ScanRequired || body.ReadinessVersion != 2 || body.SourceUploadRecoveryVersion != 1 {
			t.Fatalf("preflight %d %s %v", w.Code, w.Body, err)
		}
	}
}

func TestLineSourceReadinessNeverClaimsScannedOrClean(t *testing.T) {
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers[assets.LineHarnessCaller] = true
	h.workloadAuth.config.Principals["harness"] = assets.LineHarnessCaller
	now := time.Now().UTC()
	for _, tc := range []struct {
		name        string
		change      func(*assets.Asset)
		state, kind string
	}{
		{"ready", nil, "ready", "not-scanned"},
		{"expired", func(a *assets.Asset) {
			a.Source.PublishedAt = now.Add(-assets.SourceLifetime)
			a.Source.ExpiresAt = now
		}, "rejected", ""},
		{"unknown policy", func(a *assets.Asset) { a.Source.Version = 2 }, "rejected", ""},
		{"false clean", func(a *assets.Asset) { a.ScanStatus = assets.ScanClean }, "rejected", ""},
		{"pending", func(a *assets.Asset) { a.UploadStatus = assets.UploadCreated; a.ScanStatus = assets.ScanPending }, "pending", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := assets.Asset{ID: "source", Namespace: "line.group.file", OwnerService: assets.LineHarnessCaller, UploadProfiles: assets.UploadProfiles{SecurityProfile: assets.LineSourceSecurityProfile, ProcessingProfile: "no-processing-v1"}, UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanNotRequired, ProcessingStatus: assets.ProcessingNotRequired, Source: &assets.SourcePolicy{SourceReference: assets.SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}, Version: 1, ExpiresAt: now.Add(assets.SourceLifetime)}}
			if tc.change != nil {
				tc.change(&a)
			}
			h.service = assets.NewService(&completedDerivationRepository{asset: a}, nil, "", time.Now)
			r := httptest.NewRequest(http.MethodGet, "/priv/assets/source", nil)
			r.Header.Set(workloadHeader, f.bearer(t, "first", f.firstKey, nil, f.claims("harness")))
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			var body struct {
				ProcessingReadiness assetReadiness
				SafetyEvidence      assetSafetyEvidence
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.ProcessingReadiness.Version != 2 || body.ProcessingReadiness.State != tc.state || body.SafetyEvidence.Kind != tc.kind {
				t.Fatalf("readiness %d %s %v", w.Code, w.Body, err)
			}
		})
	}
}

func TestLineHarnessCannotUseLegacyAuthenticationOnOwnedRoutes(t *testing.T) {
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers[assets.LineHarnessCaller] = true
	for _, path := range []string{"/priv/assets/source", "/priv/assets/collections"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-HHC-Caller-App-Id", assets.LineHarnessCaller)
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}
