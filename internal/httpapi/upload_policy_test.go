package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

func TestUploadPolicyRequiresVerifiedExactScopeWithoutCreatingUpload(t *testing.T) {
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers["hhc-line-function-bot"] = true
	h.workloadAuth.config.Principals["line"] = "hhc-line-function-bot"
	h.workloadAuth.config.DisableLegacyCallers = nil
	h.workloadAuth.config.UploadProfileScopes = map[string][]assets.UploadProfileScope{
		"line": {{NamespaceFamily: "line.group.media-sync", UploadProfiles: assets.UploadProfiles{SecurityProfile: "line-media-v1", ProcessingProfile: "no-processing-v1"}}},
	}
	// No service/repository: a policy query must not perform asset or scan writes.
	for _, tc := range []struct {
		name, principal, body string
		wrongAudience         bool
		want                  int
	}{
		{"verified exact pair", "line", `{"namespace":"line.group.media-sync","securityProfile":"line-media-v1","processingProfile":"no-processing-v1"}`, false, 200},
		{"header only", "", `{"namespace":"line.group.media-sync","securityProfile":"line-media-v1","processingProfile":"no-processing-v1"}`, false, 403},
		{"wrong audience", "line", `{"namespace":"line.group.media-sync","securityProfile":"line-media-v1","processingProfile":"no-processing-v1"}`, true, 401},
		{"unrelated principal", "cms", `{"namespace":"line.group.media-sync","securityProfile":"line-media-v1","processingProfile":"no-processing-v1"}`, false, 403},
		{"unapproved namespace", "line", `{"namespace":"line.group.file","securityProfile":"line-attachment-v1","processingProfile":"no-processing-v1"}`, false, 403},
		{"unimplemented unscanned profile", "line", `{"namespace":"line.group.media-sync","securityProfile":"line-source-unscanned-v1","processingProfile":"no-processing-v1"}`, false, 400},
		{"no implicit legacy pair", "line", `{"namespace":"line.group.media-sync"}`, false, 400},
		{"partial pair", "line", `{"namespace":"line.group.media-sync","securityProfile":"line-media-v1"}`, false, 400},
		{"skip scan rejected", "line", `{"namespace":"line.group.media-sync","securityProfile":"line-media-v1","processingProfile":"no-processing-v1","skipScan":true}`, false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-policy", strings.NewReader(tc.body))
			if tc.principal == "" {
				r.Header.Set("X-HHC-Caller-App-Id", "hhc-line-function-bot")
			} else {
				claims := f.claims(tc.principal)
				if tc.wrongAudience {
					claims["aud"] = "wrong"
				}
				r.Header.Set(workloadHeader, f.bearer(t, "first", f.firstKey, nil, claims))
			}
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 {
				return
			}
			var result struct {
				Version   int    `json:"version"`
				Namespace string `json:"namespace"`
				assets.UploadProfiles
				ScanRequired     bool     `json:"scanRequired"`
				MaxSizeBytes     int64    `json:"maxSizeBytes"`
				MIMETypes        []string `json:"mimeTypes"`
				ReadinessVersion int      `json:"readinessVersion"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Version != 1 || result.Namespace != "line.group.media-sync" || result.SecurityProfile != "line-media-v1" || result.ProcessingProfile != "no-processing-v1" || !result.ScanRequired || result.MaxSizeBytes != 209715200 || result.ReadinessVersion != 1 || len(result.MIMETypes) == 0 {
				t.Fatalf("incorrect policy: %+v", result)
			}
			if w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("policy may be cached")
			}
		})
	}
}
