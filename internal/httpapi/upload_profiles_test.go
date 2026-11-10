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

func TestVerifiedUploadProfilesKeepLegacyAndAuthorizeExactNewFamily(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	profiles := assets.UploadProfiles{SecurityProfile: "web-image-v1", ProcessingProfile: "image-variants-v1"}
	h.workloadAuth.config.DisableLegacyCallers = nil
	h.workloadAuth.config.UploadProfileScopes = map[string][]assets.UploadProfileScope{
		"cms": {{NamespaceFamily: "cms.page", UploadProfiles: profiles}},
	}
	base := assets.CreateUploadInput{
		Namespace: "cms.page.image", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "page-1",
		Purpose: "page_image", ExpectedMIMEType: "image/png", OriginalFileName: "image.png",
		MaxSizeBytes: 1024, Visibility: assets.VisibilityPrivate,
	}
	for _, test := range []struct {
		name, namespace, principal string
		profiles                   assets.UploadProfiles
		want                       int
	}{
		{"legacy exact namespace", "cms.page.image", "", assets.UploadProfiles{}, 201},
		{"legacy unknown namespace", "cms.page.hero", "", assets.UploadProfiles{}, 403},
		{"legacy cannot choose explicit profile", "cms.page.image", "", profiles, 403},
		{"verified exact mapped profile", "cms.page.image", "cms", profiles, 201},
		{"verified new CMS purpose", "cms.page.hero", "cms", profiles, 201},
		{"matched namespace but unknown pair", "cms.page.image", "cms", assets.UploadProfiles{SecurityProfile: "unknown", ProcessingProfile: "image-variants-v1"}, 400},
		{"mapped namespace mismatches pair", "cms.home.banner", "cms", profiles, 400},
		{"explicit pair incomplete", "cms.page.hero", "cms", assets.UploadProfiles{SecurityProfile: "web-image-v1"}, 400},
		{"verified principal without scope", "cms.page.hero", "extractor", profiles, 403},
		{"verified prefix without dot boundary", "cms.pages.hero", "cms", profiles, 403},
		{"verified cross family", "cms.news.hero", "cms", profiles, 403},
		{"unsafe namespace", "../cms.page.hero", "cms", profiles, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &extractorRepository{}
			blobs := &extractorBlobStore{}
			h.service = assets.NewService(repository, blobs, "", func() time.Time { return fixture.now })
			input := base
			input.Namespace, input.UploadProfiles = test.namespace, test.profiles
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", strings.NewReader(string(raw)))
			r.Header.Set("X-HHC-Caller-App-Id", "hhc-web-api")
			r.Header.Set("Idempotency-Key", "fixture")
			if test.principal != "" {
				r.Header.Set(workloadHeader, fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims(test.principal)))
			}
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.want, w.Body.String())
			}
			if (repository.uploads == 1) != (test.want == 201) {
				t.Fatalf("uploads=%d", repository.uploads)
			}
			if w.Code == 201 && (strings.Contains(w.Body.String(), "securityProfile") || strings.Contains(w.Body.String(), "processingProfile")) {
				t.Fatalf("success body changed: %s", w.Body.String())
			}
		})
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{`,"securityProfile":"","processingProfile":""`, `,"securityProfile":null,"processingProfile":null`} {
		request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", strings.NewReader(strings.TrimSuffix(string(raw), "}")+extra+"}"))
		request.Header.Set("X-HHC-Caller-App-Id", "hhc-web-api")
		request.Header.Set("Idempotency-Key", "fixture")
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, request)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("explicit empty profile status=%d body=%s", w.Code, w.Body.String())
		}
	}

}
