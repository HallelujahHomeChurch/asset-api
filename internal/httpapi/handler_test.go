package httpapi

import (
	"bytes"
	"context"
	"io"
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
	return New(service, nil, nil, false, "", nil).Routes(), blobs
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
