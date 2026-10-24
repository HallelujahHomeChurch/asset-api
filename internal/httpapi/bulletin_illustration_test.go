package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type illustrationReadRepository struct {
	*downloadRepository
	reads, writes int
}

func (r *illustrationReadRepository) GetAsset(ctx context.Context, id string) (assets.Asset, error) {
	r.reads++
	return r.downloadRepository.GetAsset(ctx, id)
}
func (r *illustrationReadRepository) CreateGrant(context.Context, assets.Grant) (assets.Grant, error) {
	r.writes++
	return assets.Grant{}, assets.ErrForbidden
}
func (r *illustrationReadRepository) RevokeGrant(context.Context, string, string, time.Time) error {
	r.writes++
	return assets.ErrForbidden
}

func TestBulletinIllustrationReadsCheckFreshAuthorityWithoutGrantWrites(t *testing.T) {
	for _, kind := range []string{"valid", "caller", "token", "owner", "issue", "namespace", "purpose", "locale", "public", "deleted", "checksum", "mime", "upload", "scan", "processing", "size"} {
		t.Run(kind, func(t *testing.T) {
			asset := assets.Asset{ID: "asset-1", Namespace: "cms.weekly.image", OwnerService: "hhc-web-api", OwnerType: "bulletin_issue", OwnerID: "issue-1", Purpose: "online_bulletin_illustration", Locale: "zh-Hant", ObjectKey: "image", DetectedMIMEType: "image/png", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), ETag: `"image"`, UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean, ProcessingStatus: assets.ProcessingNotRequired, Visibility: assets.VisibilityPrivate}
			switch kind {
			case "owner":
				asset.OwnerService = "account-api"
			case "issue":
				asset.OwnerID = "other-issue"
			case "namespace":
				asset.Namespace = "cms.weekly.pdf"
			case "purpose":
				asset.Purpose = "weekly_bulletin"
			case "locale":
				asset.Locale = "zh-Hans"
			case "public":
				asset.Visibility = assets.VisibilityPublic
			case "deleted":
				asset.DeletedAt = time.Now()
			case "checksum":
				asset.ChecksumSHA256 = strings.Repeat("b", 64)
			case "mime":
				asset.DetectedMIMEType = "application/pdf"
			case "upload":
				asset.UploadStatus = assets.UploadCreated
			case "scan":
				asset.ScanStatus = assets.ScanPending
			case "processing":
				asset.ProcessingStatus = assets.ProcessingPending
			case "size":
				asset.SizeBytes = 2<<20 + 1
			}
			repo := &illustrationReadRepository{downloadRepository: &downloadRepository{asset: asset}}
			blobs := &downloadBlobStore{objects: map[string][]byte{"image": []byte("image")}}
			handler := New(assets.NewService(repo, blobs, "", time.Now), nil, map[string]bool{"hhc-web-api": true, "account-api": true}, false, "token", WorkloadAuthConfig{}, nil).Routes()
			read := func() *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodGet, "/priv/assets/asset-1/bulletin-illustration?issueId=issue-1&checksumSha256="+strings.Repeat("a", 64), nil)
				request.Header.Set("Dapr-Caller-App-Id", "hhc-web-api")
				request.Header.Set("dapr-api-token", "token")
				if kind == "caller" {
					request.Header.Set("Dapr-Caller-App-Id", "account-api")
				}
				if kind == "token" {
					request.Header.Set("dapr-api-token", "wrong")
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			for range 2 {
				response := read()
				if kind == "valid" {
					if response.Code != http.StatusOK || response.Body.String() != "image" || response.Header().Get("Cache-Control") != "private, no-store" {
						t.Fatal("owned image unavailable", response.Code)
					}
				} else if response.Code < 400 || blobs.openCalls != 0 {
					t.Fatal("untrusted image opened", response.Code, blobs.openCalls)
				}
			}
			if kind == "valid" {
				if repo.reads != 2 || blobs.openCalls != 2 {
					t.Fatal("expected one fresh metadata read per request", repo.reads, blobs.openCalls)
				}
				repo.asset.ScanStatus = assets.ScanPending
				if response := read(); response.Code < 400 || blobs.openCalls != 2 {
					t.Fatal("changed scan status was cached", response.Code)
				}
			}
			if repo.writes != 0 {
				t.Fatal("content read wrote grants", repo.writes)
			}
		})
	}
}
