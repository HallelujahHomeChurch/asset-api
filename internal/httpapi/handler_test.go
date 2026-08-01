package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestParseRange(t *testing.T) {
	value, partial, err := parseRange("bytes=10-19")
	if err != nil || !partial || value.Offset != 10 || value.Count != 10 {
		t.Fatalf("range=%+v partial=%v err=%v", value, partial, err)
	}
	value, partial, err = parseRange("bytes=10-")
	if err != nil || !partial || value.Offset != 10 || value.Count != 0 {
		t.Fatalf("open range=%+v partial=%v err=%v", value, partial, err)
	}
}

func TestParseRangeAcceptsSuffix(t *testing.T) {
	value, partial, err := parseRange("bytes=-10")
	if err != nil || !partial || value.Suffix != 10 {
		t.Fatalf("range=%+v partial=%v err=%v", value, partial, err)
	}
}

func TestParseRangeRejectsInvalidSuffixAndMultipleRanges(t *testing.T) {
	for _, input := range []string{"bytes=-0", "bytes=-9223372036854775808", "bytes=0-1,3-4", "items=0-1"} {
		if _, _, err := parseRange(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestProductionCallerUsesDaprIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("Dapr-Caller-App-Id", "account-api")
	request.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")

	if caller := callerFromRequest(request, false); caller != "account-api" {
		t.Fatalf("caller = %q", caller)
	}
}

func TestProductionCallerRejectsDevelopmentHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-Internal-Caller-App-Id", "account-api")

	if caller := callerFromRequest(request, false); caller != "" {
		t.Fatalf("caller = %q", caller)
	}
	if caller := callerFromRequest(request, true); caller != "account-api" {
		t.Fatalf("development caller = %q", caller)
	}
}

func TestInternalRequiresAppTokenInProduction(t *testing.T) {
	handler := (&Handler{
		allowedCallers: map[string]bool{"account-api": true},
		appAPIToken:    "secret",
	}).internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "missing", want: http.StatusForbidden},
		{name: "wrong", token: "wrong", want: http.StatusForbidden},
		{name: "valid", token: "secret", want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
			request.Header.Set("Dapr-Caller-App-Id", "account-api")
			request.Header.Set("dapr-api-token", test.token)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestInternalAllowsExplicitDevelopmentCaller(t *testing.T) {
	handler := (&Handler{
		allowedCallers:       map[string]bool{"account-api": true},
		allowDevCallerHeader: true,
	}).internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-Internal-Caller-App-Id", "account-api")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestInternalAllowsValidatedEntraWorkload(t *testing.T) {
	auth := WorkloadAuthConfig{
		TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://login.microsoftonline.com/tenant-1/v2.0",
		RequiredRole: "Asset.Invoke", Callers: map[string]WorkloadCaller{
			"line-client": {ObjectID: "line-object", Service: "hhc-line-function-bot"},
		},
	}
	handler := (&Handler{allowedCallers: map[string]bool{"hhc-line-function-bot": true}, workloadAuth: auth}).internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callerFromRequest(r, false) != "hhc-line-function-bot" {
			t.Fatalf("caller = %q", callerFromRequest(r, false))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, map[string]string{
		"tid": "tenant-1", "iss": auth.Issuer, "aud": auth.Audience, "appid": "line-client", "oid": "line-object", "roles": auth.RequiredRole,
	}))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestManagedIdentityV1IssuerIsLimitedToConfiguredTenant(t *testing.T) {
	config := WorkloadAuthConfig{
		TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://login.microsoftonline.com/tenant-1/v2.0",
		RequiredRole: "Asset.Invoke", Callers: map[string]WorkloadCaller{"line-client": {ObjectID: "line-object", Service: "hhc-line-function-bot"}},
	}
	claims := map[string]string{
		"tid": "tenant-1", "iss": "https://sts.windows.net/tenant-1/", "aud": config.Audience,
		"appid": "line-client", "oid": "line-object", "roles": config.RequiredRole,
	}
	if caller := workloadCaller(encodedPrincipal(t, claims), config); caller != "hhc-line-function-bot" {
		t.Fatalf("caller = %q", caller)
	}
	claims["iss"] = "https://sts.windows.net/other-tenant/"
	if caller := workloadCaller(encodedPrincipal(t, claims), config); caller != "" {
		t.Fatalf("cross-tenant caller = %q", caller)
	}
}

func TestInternalRejectsInvalidEntraWorkloadClaims(t *testing.T) {
	auth := WorkloadAuthConfig{
		TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://login.microsoftonline.com/tenant-1/v2.0", RequiredRole: "Asset.Invoke",
		Callers: map[string]WorkloadCaller{"line-client": {ObjectID: "line-object", Service: "hhc-line-function-bot"}},
	}
	base := map[string]string{"tid": auth.TenantID, "iss": auth.Issuer, "aud": auth.Audience, "appid": "line-client", "oid": "line-object", "roles": auth.RequiredRole}
	for _, claim := range []string{"tid", "iss", "aud", "appid", "oid", "roles"} {
		t.Run(claim, func(t *testing.T) {
			claims := maps.Clone(base)
			claims[claim] = "wrong"
			handler := (&Handler{allowedCallers: map[string]bool{"hhc-line-function-bot": true}, workloadAuth: auth}).internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
			request.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, claims))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
}

func encodedPrincipal(t *testing.T, claims map[string]string) string {
	t.Helper()
	value := struct {
		AuthType string `json:"auth_typ"`
		Claims   []struct {
			Type  string `json:"typ"`
			Value string `json:"val"`
		} `json:"claims"`
	}{AuthType: "aad"}
	for key, claim := range claims {
		value.Claims = append(value.Claims, struct {
			Type  string `json:"typ"`
			Value string `json:"val"`
		}{Type: key, Value: claim})
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func TestLocalUploadCORSAllowsAdminDevelopmentOrigin(t *testing.T) {
	nextCalled := false
	handler := localUploadCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodOptions, "/dev/uploads/token", nil)
	request.Header.Set("Origin", "http://127.0.0.1:5175")
	request.Header.Set("Access-Control-Request-Method", http.MethodPut)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || nextCalled {
		t.Fatalf("status=%d next=%v", response.Code, nextCalled)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5175" {
		t.Fatalf("allow origin=%q", response.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestPublicDownloadHeadMatchesGetWithoutOpeningBlob(t *testing.T) {
	for _, variant := range []string{"", "small"} {
		t.Run(variant, func(t *testing.T) {
			headHandler, headBlobs := publicDownloadHandler(t)
			path := "/api/assets/public/asset-1"
			if variant != "" {
				path += "/" + variant
			}
			head := httptest.NewRecorder()
			headHandler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))

			getHandler, _ := publicDownloadHandler(t)
			get := httptest.NewRecorder()
			getHandler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))

			if head.Code != get.Code || head.Code != http.StatusOK {
				t.Fatalf("HEAD=%d GET=%d", head.Code, get.Code)
			}
			for _, name := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Cache-Control", "Accept-Ranges"} {
				if head.Header().Get(name) != get.Header().Get(name) {
					t.Fatalf("%s: HEAD=%q GET=%q", name, head.Header().Get(name), get.Header().Get(name))
				}
			}
			if head.Body.Len() != 0 || headBlobs.openCalls != 0 {
				t.Fatalf("HEAD body=%q openCalls=%d", head.Body.String(), headBlobs.openCalls)
			}
		})
	}
}

func TestPublicDownloadNotModifiedDoesNotOpenBlob(t *testing.T) {
	for _, test := range []struct {
		path  string
		value string
	}{
		{path: "/api/assets/public/asset-1", value: `W/"other", W/"original"`},
		{path: "/api/assets/public/asset-1/small", value: `W/"small"`},
		{path: "/api/assets/public/asset-1", value: "*"},
	} {
		handler, blobs := publicDownloadHandler(t)
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		request.Header.Set("If-None-Match", test.value)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusNotModified || response.Body.Len() != 0 || blobs.openCalls != 0 {
			t.Fatalf("If-None-Match=%q status=%d body=%q openCalls=%d", test.value, response.Code, response.Body.String(), blobs.openCalls)
		}
	}
}

func TestPublicDownloadRangeUsesStoredTotal(t *testing.T) {
	handler, blobs := publicDownloadHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
	request.Header.Set("Range", "bytes=1-99")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent || response.Header().Get("Content-Range") != "bytes 1-5/6" || response.Header().Get("Content-Length") != "5" || response.Body.String() != "bcdef" || blobs.openCalls != 1 {
		t.Fatalf("status=%d headers=%v body=%q openCalls=%d", response.Code, response.Header(), response.Body.String(), blobs.openCalls)
	}
}

func TestPublicDownloadSuffixRange(t *testing.T) {
	for _, test := range []struct {
		value        string
		contentRange string
		body         string
	}{
		{value: "bytes=-2", contentRange: "bytes 4-5/6", body: "ef"},
		{value: "bytes=-99", contentRange: "bytes 0-5/6", body: "abcdef"},
	} {
		handler, blobs := publicDownloadHandler(t)
		request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
		request.Header.Set("Range", test.value)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusPartialContent || response.Header().Get("Content-Range") != test.contentRange || response.Body.String() != test.body || blobs.openCalls != 1 {
			t.Fatalf("range=%q status=%d content-range=%q body=%q openCalls=%d", test.value, response.Code, response.Header().Get("Content-Range"), response.Body.String(), blobs.openCalls)
		}
	}
}

func TestPublicDownloadIfRangeControlsPartialResponse(t *testing.T) {
	modified := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		validator string
		status    int
		body      string
	}{
		{name: "matching etag", validator: `"original"`, status: http.StatusPartialContent, body: "bc"},
		{name: "mismatched etag", validator: `"other"`, status: http.StatusOK, body: "abcdef"},
		{name: "weak etag", validator: `W/"original"`, status: http.StatusOK, body: "abcdef"},
		{name: "matching date", validator: modified.Format(http.TimeFormat), status: http.StatusPartialContent, body: "bc"},
		{name: "older date", validator: modified.Add(-time.Second).Format(http.TimeFormat), status: http.StatusOK, body: "abcdef"},
		{name: "invalid date", validator: "not-a-validator", status: http.StatusOK, body: "abcdef"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, blobs := publicDownloadHandler(t)
			request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
			request.Header.Set("Range", "bytes=1-2")
			request.Header.Set("If-Range", test.validator)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.status || response.Body.String() != test.body || blobs.openCalls != 1 {
				t.Fatalf("status=%d body=%q openCalls=%d", response.Code, response.Body.String(), blobs.openCalls)
			}
			if test.status == http.StatusOK && response.Header().Get("Content-Range") != "" {
				t.Fatalf("full response has Content-Range %q", response.Header().Get("Content-Range"))
			}
		})
	}
}

func TestPublicDownloadSuffixRangeRejectsEmptyRepresentation(t *testing.T) {
	handler, blobs := publicDownloadHandlerWithOriginal(t, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
	request.Header.Set("Range", "bytes=-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestedRangeNotSatisfiable || response.Header().Get("Content-Range") != "bytes */0" || blobs.openCalls != 0 {
		t.Fatalf("status=%d content-range=%q openCalls=%d", response.Code, response.Header().Get("Content-Range"), blobs.openCalls)
	}
}

func TestPublicDownloadUnsatisfiedRangeDoesNotOpenBlob(t *testing.T) {
	for _, value := range []string{"bytes=6-", "bytes=-0", "bytes=-9223372036854775808", "bytes=0-1,3-4", "bytes=0-9223372036854775807"} {
		handler, blobs := publicDownloadHandler(t)
		request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
		request.Header.Set("Range", value)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusRequestedRangeNotSatisfiable || response.Header().Get("Content-Range") != "bytes */6" || blobs.openCalls != 0 {
			t.Fatalf("range=%q status=%d content-range=%q openCalls=%d", value, response.Code, response.Header().Get("Content-Range"), blobs.openCalls)
		}
	}
}

func TestAssetErrorsAreNotCacheable(t *testing.T) {
	handler, _ := publicDownloadHandler(t)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/assets/public/missing", nil),
		httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil),
	} {
		if request.URL.Path != "/api/assets/public/missing" {
			request.Header.Set("Range", "bytes=99-100")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("path=%s status=%d cache=%q", request.URL.Path, response.Code, response.Header().Get("Cache-Control"))
		}
	}
}

func TestRestrictedDownloadRequiresOwnerAndSubjectGrant(t *testing.T) {
	modified := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	repository := &downloadRepository{
		asset: assets.Asset{
			ID: "asset-1", Namespace: "line.group.file", OwnerService: "hhc-line-function-bot",
			ObjectKey: "original", DetectedMIMEType: "application/pdf", SizeBytes: 6, ETag: `"original"`,
			UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean,
			ProcessingStatus: assets.ProcessingNotRequired, Visibility: assets.VisibilityRestricted, UpdatedAt: modified,
		},
	}
	blobs := &downloadBlobStore{objects: map[string][]byte{"original": []byte("secret")}}
	service := assets.NewService(repository, blobs, "https://www.alive.org.tw/api/assets", func() time.Time { return modified })
	handler := New(service, nil, map[string]bool{"hhc-line-function-bot": true}, false, "token", WorkloadAuthConfig{}, nil).Routes()

	request := httptest.NewRequest(http.MethodGet, "/priv/assets/asset-1/download", nil)
	request.Header.Set("Dapr-Caller-App-Id", "hhc-line-function-bot")
	request.Header.Set("dapr-api-token", "token")
	request.Header.Set("X-Asset-Subject-Type", "line_group")
	request.Header.Set("X-Asset-Subject-Id", "group-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "secret" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache control=%q", response.Header().Get("Cache-Control"))
	}
}

func publicDownloadHandler(t *testing.T) (http.Handler, *downloadBlobStore) {
	t.Helper()
	return publicDownloadHandlerWithOriginal(t, []byte("abcdef"))
}

func publicDownloadHandlerWithOriginal(t *testing.T, original []byte) (http.Handler, *downloadBlobStore) {
	t.Helper()
	modified := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	repository := &downloadRepository{
		asset: assets.Asset{
			ID: "asset-1", Namespace: "cms.news.cover", ObjectKey: "original", DetectedMIMEType: "image/jpeg",
			SizeBytes: int64(len(original)), ETag: `"original"`, UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean,
			ProcessingStatus: assets.ProcessingReady, Visibility: assets.VisibilityPublic, UpdatedAt: modified,
		},
		derivative: assets.Derivative{
			AssetID: "asset-1", Variant: "small", ObjectKey: "small", MIMEType: "image/jpeg",
			SizeBytes: 3, ETag: `"small"`, CreatedAt: modified,
		},
	}
	blobs := &downloadBlobStore{objects: map[string][]byte{"original": original, "small": []byte("xyz")}}
	service := assets.NewService(repository, blobs, "https://www.alive.org.tw/api/assets", func() time.Time { return modified })
	return New(service, nil, nil, false, "", WorkloadAuthConfig{}, nil).Routes(), blobs
}

type downloadRepository struct {
	assets.Repository
	asset      assets.Asset
	derivative assets.Derivative
}

func (r *downloadRepository) GetAsset(_ context.Context, id string) (assets.Asset, error) {
	if id != r.asset.ID {
		return assets.Asset{}, assets.ErrNotFound
	}
	return r.asset, nil
}
func (r *downloadRepository) HasActiveGrant(context.Context, string, assets.SubjectType, string, assets.Permission, time.Time) (bool, error) {
	return true, nil
}
func (r *downloadRepository) GetDerivative(_ context.Context, assetID, variant string) (assets.Derivative, error) {
	if assetID != r.derivative.AssetID || variant != r.derivative.Variant {
		return assets.Derivative{}, assets.ErrNotFound
	}
	return r.derivative, nil
}

type downloadBlobStore struct {
	assets.BlobStore
	objects   map[string][]byte
	openCalls int
}

func (b *downloadBlobStore) Open(_ context.Context, key string, byteRange assets.ByteRange, _ string) (assets.BlobDownload, error) {
	b.openCalls++
	value, ok := b.objects[key]
	if !ok {
		return assets.BlobDownload{}, assets.ErrNotFound
	}
	if byteRange.Offset > 0 || byteRange.Count > 0 {
		value = value[byteRange.Offset : byteRange.Offset+byteRange.Count]
	}
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader(value)), Size: int64(len(value))}, nil
}
