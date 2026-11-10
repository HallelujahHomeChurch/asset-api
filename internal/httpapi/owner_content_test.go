package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestVerifiedOwnerContentChecksScopeAndFencesTheStream(t *testing.T) {
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	h.workloadAuth.config.DisableLegacyCallers = nil
	const objectKey = "synthetic/private-source"
	hash := strings.Repeat("a", 64)
	base := assets.Asset{
		ID: "asset", Namespace: "cms.weekly.image", OwnerService: "hhc-web-api",
		OwnerType: "bulletin_issue", OwnerID: "issue", Purpose: "online_bulletin_illustration", Locale: "en",
		Visibility: assets.VisibilityPrivate, ObjectKey: objectKey, DetectedMIMEType: "image/png", SizeBytes: 6,
		ChecksumSHA256: hash, ETag: `"original"`, UploadStatus: assets.UploadCompleted,
		ScanStatus: assets.ScanClean, ProcessingStatus: assets.ProcessingNotRequired,
	}
	path := "/priv/assets/asset/owner-content?ownerType=bulletin_issue&ownerId=issue&sha256=" + hash
	for _, test := range []struct {
		name, method, path, principal, rangeValue, wantBody string
		disabled, changeETag, conditional                   bool
		before, during                                      func(*assets.Asset)
		dependency                                          int
		want, reads, opens, closes                          int
	}{
		{name: "cross locale PNG", want: 200, reads: 2, opens: 1, closes: 1, wantBody: "abcdef"},
		{name: "PDF source", before: func(a *assets.Asset) {
			a.Namespace, a.Purpose, a.DetectedMIMEType = "cms.weekly.pdf", "weekly_bulletin", "application/pdf"
		}, want: 200, reads: 2, opens: 1, closes: 1, wantBody: "abcdef"},
		{name: "range", rangeValue: "bytes=1-3", want: 206, reads: 2, opens: 1, closes: 1, wantBody: "bcd"},
		{name: "HEAD", method: http.MethodHead, want: 200, reads: 1},
		{name: "conditional", conditional: true, want: 304, reads: 1},
		{name: "scope disabled", disabled: true, want: 403},
		{name: "legacy caller", principal: "legacy", want: 403},
		{name: "unapproved principal", principal: "extractor", want: 403},
		{name: "missing owner", path: "/priv/assets/asset/owner-content?sha256=" + hash, want: 400},
		{name: "duplicate owner", path: path + "&ownerId=other", want: 400},
		{name: "wrong digest", path: strings.Replace(path, hash, strings.Repeat("b", 64), 1), want: 404, reads: 1},
		{name: "wrong owner", path: strings.Replace(path, "ownerId=issue", "ownerId=wrong", 1), want: 404, reads: 1},
		{name: "other service", before: func(a *assets.Asset) { a.OwnerService = "account-api" }, want: 404, reads: 1},
		{name: "collection namespace", before: func(a *assets.Asset) { a.Namespace = "line.group.media-sync" }, want: 404, reads: 1},
		{name: "derived job owner", before: func(a *assets.Asset) { a.OwnerType = "bulletin_download_job" }, want: 404, reads: 1},
		{name: "purged", before: func(a *assets.Asset) { a.PurgedAt = time.Now() }, want: 404, reads: 1},
		{name: "scan pending", before: func(a *assets.Asset) { a.ScanStatus = assets.ScanPending }, want: 404, reads: 1},
		{name: "infected", before: func(a *assets.Asset) { a.ScanStatus = assets.ScanInfected }, want: 404, reads: 1},
		{name: "deleted during Blob open", during: func(a *assets.Asset) { a.DeletedAt = time.Now() }, want: 404, reads: 2, opens: 1, closes: 1},
		{name: "quarantined during Blob open", during: func(a *assets.Asset) { a.ScanStatus = assets.ScanInfected }, want: 404, reads: 2, opens: 1, closes: 1},
		{name: "immutable version changed", changeETag: true, want: 404, reads: 1, opens: 1, closes: 1},
		{name: "metadata dependency", dependency: 1, want: 500, reads: 1},
		{name: "dependency after Blob open", dependency: 2, want: 500, reads: 2, opens: 1, closes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			asset := base
			if test.before != nil {
				test.before(&asset)
			}
			repo := &ownerContentRepository{
				illustrationReadRepository: illustrationReadRepository{downloadRepository: &downloadRepository{asset: asset}},
			}
			if test.dependency == 1 {
				repo.err = errors.New("synthetic confidential database error")
			}
			blobs := &ownerContentBlobStore{
				downloadBlobStore: downloadBlobStore{objects: map[string][]byte{objectKey: []byte("abcdef")}},
				changeETag:        test.changeETag,
			}
			blobs.afterOpen = func() {
				if test.during != nil {
					test.during(&repo.asset)
				}
				if test.dependency == 2 {
					repo.err = errors.New("synthetic confidential database error")
				}
			}
			h.service = assets.NewService(repo, blobs, "", time.Now)
			h.workloadAuth.config.OwnerContentPrincipals = nil
			if !test.disabled {
				h.workloadAuth.config.OwnerContentPrincipals = []string{"cms"}
			}
			target, method, principal := test.path, test.method, test.principal
			if target == "" {
				target = path
			}
			if method == "" {
				method = http.MethodGet
			}
			if principal == "" {
				principal = "cms"
			}
			request := httptest.NewRequest(method, target, nil)
			request.Header.Set("X-HHC-Caller-App-Id", "hhc-web-api")
			if principal != "legacy" {
				request.Header.Set(workloadHeader, fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims(principal)))
			}
			request.Header.Set("Range", test.rangeValue)
			if test.conditional {
				request.Header.Set("If-None-Match", base.ETag)
			}
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, request)
			if response.Code != test.want || repo.reads != test.reads || repo.writes != 0 || blobs.openCalls != test.opens || blobs.closes != test.closes {
				t.Fatalf("status=%d reads=%d writes=%d opens=%d closes=%d body=%s",
					response.Code, repo.reads, repo.writes, blobs.openCalls, blobs.closes, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "private, no-store" ||
				strings.Contains(response.Body.String(), objectKey) || strings.Contains(response.Body.String(), "confidential") ||
				response.Header().Get("Location") != "" {
				t.Fatalf("unsafe response: headers=%v body=%s", response.Header(), response.Body.String())
			}
			if test.want == 206 && response.Header().Get("Content-Range") != "bytes 1-3/6" {
				t.Fatalf("range headers=%v", response.Header())
			}
			if test.want < 400 && response.Body.String() != test.wantBody {
				t.Fatalf("body=%q want=%q", response.Body.String(), test.wantBody)
			}
		})
	}
}

type ownerContentRepository struct {
	illustrationReadRepository
	err error
}

func (r *ownerContentRepository) GetAsset(ctx context.Context, id string) (assets.Asset, error) {
	if r.err != nil {
		r.reads++
		return assets.Asset{}, r.err
	}
	return r.illustrationReadRepository.GetAsset(ctx, id)
}

type ownerContentBlobStore struct {
	downloadBlobStore
	afterOpen  func()
	changeETag bool
	closes     int
}

func (b *ownerContentBlobStore) Open(ctx context.Context, key string, rangeValue assets.ByteRange, etag string) (assets.BlobDownload, error) {
	result, err := b.downloadBlobStore.Open(ctx, key, rangeValue, etag)
	if err != nil {
		return result, err
	}
	result.ETag = etag
	result.TotalSize = int64(len(b.objects[key]))
	if b.changeETag {
		result.ETag = `"different"`
	}
	if b.afterOpen != nil {
		b.afterOpen()
	}
	result.Body = &ownerContentTestBody{ReadCloser: result.Body, onClose: func() { b.closes++ }}
	return result, nil
}

type ownerContentTestBody struct {
	io.ReadCloser
	onClose func()
}

func (b *ownerContentTestBody) Close() error {
	b.onClose()
	return b.ReadCloser.Close()
}
