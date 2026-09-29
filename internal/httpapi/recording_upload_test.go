package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type recordingAssetRepository struct{ session assets.RecordingUploadSession }

func TestRecordingAssetExposesAuthoritativeCompletion(t *testing.T) {
	at := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	repo := recordingAssetRepository{assets.RecordingUploadSession{OwnerService: "hhc-web-api", RecordingID: "rec-a", AssetVersionID: "version-a", Status: "ready", UploadedAt: &at}}
	handler := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingGrants(repo, nil).Routes()
	req := httptest.NewRequest(http.MethodGet, "/priv/recording-assets/version-a", nil)
	req.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body["uploadedAt"] != "2026-09-28T02:00:00Z" {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
}

func (r recordingAssetRepository) GetByVersion(_ context.Context, _ string) (assets.RecordingUploadSession, error) {
	return r.session, nil
}

func TestRecordingUploadRoutesRemainPrivateAndFailClosed(t *testing.T) {
	handler := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, tc := range []struct {
		caller string
		want   int
	}{
		{"", http.StatusForbidden},
		{"account-api", http.StatusForbidden},
		{"hhc-web-api", http.StatusServiceUnavailable},
	} {
		req := httptest.NewRequest(http.MethodPost, "/priv/recording-uploads", strings.NewReader(`{}`))
		if tc.caller != "" {
			req.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatalf("caller=%q status=%d want=%d", tc.caller, response.Code, tc.want)
		}
	}
}

func TestRecordingGrantRequiresReadyMatchingVersion(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := assets.NewRecordingSigner(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "test-key", "hhc-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		want   int
	}{{"validating", http.StatusForbidden}, {"ready", http.StatusOK}} {
		repo := recordingAssetRepository{assets.RecordingUploadSession{OwnerService: "hhc-web-api", RecordingID: "rec-a", AssetVersionID: "version-a", ObjectKey: "recordings/version-a.mp4", Status: tc.status}}
		handler := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingGrants(repo, signer).Routes()
		body := `{"recordingId":"rec-a","scopeId":"scope-a","recordingExpiresAt":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/priv/recording-assets/version-a/grants", strings.NewReader(body))
		req.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatalf("status=%q got=%d body=%s", tc.status, response.Code, response.Body.String())
		}
	}
}
