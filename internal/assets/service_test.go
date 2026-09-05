package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompleteUploadValidatesObservedBlobAndLeavesDownloadPending(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)

	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-1", Purpose: "pdf", Locale: "zh-Hant", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 5 << 20, Visibility: VisibilityPublic,
	}, "request-1")
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("%PDF-1.7\nweekly bulletin")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.ScanStatus != ScanPending {
		t.Fatalf("scan status = %s", asset.ScanStatus)
	}
	var request ScanRequest
	for _, candidate := range repo.scanRequests {
		if candidate.AssetID == asset.ID {
			request = candidate
		}
	}
	if request.EventID == "" || request.AssetID != asset.ID || request.ETag != asset.ETag {
		t.Fatalf("scan request = %+v", request)
	}
	if created.Session.StagingObjectKey == "" || created.Session.StagingObjectKey == asset.ObjectKey {
		t.Fatalf("staging=%q final=%q", created.Session.StagingObjectKey, asset.ObjectKey)
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
		t.Fatal("staging object remains after completion")
	}
	if _, ok := blobs.objects[asset.ObjectKey]; !ok {
		t.Fatal("final object is missing after completion")
	}
	if _, err := service.OpenPublic(ctx, asset.ID, ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public download before clean scan: %v", err)
	}
}

func TestCreateHomeBannerUploadRejectsWrongOwnerAndUncroppedMIME(t *testing.T) {
	service := NewService(newMemoryRepository(), newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	base := CreateUploadInput{
		Namespace: "cms.home.banner", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "home-1", Purpose: "home_banner",
		OriginalFileName: "banner.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 10 << 20, Visibility: VisibilityPublic,
	}
	for _, test := range []struct {
		name  string
		input CreateUploadInput
	}{
		{name: "owner mismatch", input: func() CreateUploadInput { value := base; value.OwnerService = "account-api"; return value }()},
		{name: "PNG", input: func() CreateUploadInput {
			value := base
			value.OriginalFileName = "banner.png"
			value.ExpectedMIMEType = "image/png"
			return value
		}()},
		{name: "WebP", input: func() CreateUploadInput {
			value := base
			value.OriginalFileName = "banner.webp"
			value.ExpectedMIMEType = "image/webp"
			return value
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.CreateUploadSession(context.Background(), test.input, "home-banner-"+test.name); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCompleteHomeBannerValidatesJPEGDimensionsFromByteZero(t *testing.T) {
	for _, test := range []struct {
		name      string
		payload   func(*testing.T) []byte
		wantError bool
	}{
		{name: "invalid JPEG", payload: func(*testing.T) []byte { return []byte{0xff, 0xd8, 0xff, 0xe0} }, wantError: true},
		{name: "MIME spoofed PNG", payload: func(t *testing.T) []byte { return encodePNG(t, 1900, 700) }, wantError: true},
		{name: "MIME spoofed WebP", payload: func(*testing.T) []byte { return []byte("RIFF\x08\x00\x00\x00WEBPVP8 ") }, wantError: true},
		{name: "1899x700", payload: func(t *testing.T) []byte { return encodeJPEG(t, 1899, 700, false) }, wantError: true},
		{name: "1900x699", payload: func(t *testing.T) []byte { return encodeJPEG(t, 1900, 699, false) }, wantError: true},
		{name: "1900x700", payload: func(t *testing.T) []byte { return encodeJPEG(t, 1900, 700, false) }},
		{name: "1900x700 with SOF after byte 512", payload: func(t *testing.T) []byte { return encodeJPEG(t, 1900, 700, true) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newMemoryRepository()
			blobs := newMemoryBlobStore()
			service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
			created, err := service.CreateUploadSession(ctx, CreateUploadInput{
				Namespace: "cms.home.banner", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "home-1", Purpose: "home_banner",
				OriginalFileName: "banner.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 10 << 20, Visibility: VisibilityPublic,
			}, "home-banner-complete-"+test.name)
			if err != nil {
				t.Fatal(err)
			}
			payload := test.payload(t)
			blobs.objects[created.Session.StagingObjectKey] = payload
			sum := sha256.Sum256(payload)
			asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
				SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/jpeg",
			})
			if test.wantError {
				if !errors.Is(err, ErrInvalidUpload) {
					t.Fatalf("error = %v", err)
				}
				if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok || repo.assets[created.Asset.ID].UploadStatus != UploadFailed {
					t.Fatal("invalid banner was not rejected and cleaned up")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if asset.UploadStatus != UploadCompleted || asset.DetectedMIMEType != "image/jpeg" || asset.ProcessingStatus != ProcessingNotRequired {
				t.Fatalf("asset = %+v", asset)
			}
			if blobs.lastOpenRange != (ByteRange{Offset: 0, Count: 10 << 20}) || blobs.lastOpenETag == "" {
				t.Fatalf("dimension read range=%+v etag=%q", blobs.lastOpenRange, blobs.lastOpenETag)
			}
		})
	}
}

func TestCompleteHomeBannerRejectsDimensionReadETagMismatchAndCleansUp(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.home.banner", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "home-1", Purpose: "home_banner",
		OriginalFileName: "banner.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 10 << 20, Visibility: VisibilityPublic,
	}, "home-banner-etag-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	payload := encodeJPEG(t, 1900, 700, false)
	blobs.objects[created.Session.StagingObjectKey] = payload
	blobs.openETagMismatchAt = 2
	sum := sha256.Sum256(payload)

	_, err = service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "image/jpeg",
	})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("error = %v", err)
	}
	if repo.assets[created.Asset.ID].UploadStatus != UploadFailed {
		t.Fatal("ETag mismatch did not mark upload failed")
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
		t.Fatal("ETag mismatch did not clean up staging object")
	}
}

func encodeJPEG(t *testing.T, width, height int, delaySOF bool) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, image.NewGray(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	payload := output.Bytes()
	if !delaySOF {
		return payload
	}
	padding := make([]byte, 600)
	segment := []byte{0xff, 0xe1, 0x02, 0x5a}
	delayed := append(append(append([]byte{}, payload[:2]...), segment...), padding...)
	delayed = append(delayed, payload[2:]...)
	if offset := bytes.Index(delayed, []byte{0xff, 0xc0}); offset <= 512 {
		t.Fatalf("SOF offset = %d", offset)
	}
	return delayed
}

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestCompleteUploadRejectsOversizeBeforeRead(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "account.avatar", OwnerService: "account-api", OwnerType: "user",
		OwnerID: "user-1", Purpose: "avatar", OriginalFileName: "avatar.jpg",
		ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 8, Visibility: VisibilityPublic,
	}, "oversize")
	if err != nil {
		t.Fatal(err)
	}
	blobs.objects[created.Session.StagingObjectKey] = []byte("too-large")

	_, err = service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: 9, ChecksumSHA256: "irrelevant", MIMEType: "image/jpeg",
	})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("error = %v", err)
	}
	if blobs.inspectCalls != 0 {
		t.Fatalf("content inspection calls = %d", blobs.inspectCalls)
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
		t.Fatal("oversize staging object was not deleted")
	}
	if repo.assets[created.Asset.ID].UploadStatus != UploadFailed || repo.sessions[created.Asset.ID].Status != UploadFailed {
		t.Fatal("oversize upload was not marked failed")
	}
}

func TestCompleteUploadRecoversAfterBlobCommitAndDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	repo.completeFailures = 1
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-1", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024, Visibility: VisibilityPublic,
	}, "recover-completion")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nrecover")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	input := CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"}

	if _, err := service.CompleteUpload(ctx, created.Asset.ID, input); err == nil {
		t.Fatal("first completion should expose the database failure")
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
		t.Fatal("staging object should already be committed")
	}
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if asset.UploadStatus != UploadCompleted {
		t.Fatalf("upload status = %q", asset.UploadStatus)
	}
}

func TestCompleteUploadRetriesOversizeFailureAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failDatabaseOnce bool
		failDeleteOnce   bool
	}{
		{name: "database failure", failDatabaseOnce: true},
		{name: "blob delete failure", failDeleteOnce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newMemoryRepository()
			if tc.failDatabaseOnce {
				repo.failUploadFailures = 1
			}
			blobs := newMemoryBlobStore()
			if tc.failDeleteOnce {
				blobs.deleteFailures = 1
			}
			service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
			created, err := service.CreateUploadSession(ctx, CreateUploadInput{
				Namespace: "account.avatar", OwnerService: "account-api", OwnerType: "user",
				OwnerID: "user-1", Purpose: "avatar", OriginalFileName: "avatar.jpg",
				ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 8, Visibility: VisibilityPublic,
			}, "oversize-retry-"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			blobs.objects[created.Session.StagingObjectKey] = []byte("too-large")
			input := CompleteUploadInput{SizeBytes: 9, ChecksumSHA256: "irrelevant", MIMEType: "image/jpeg"}

			if _, err := service.CompleteUpload(ctx, created.Asset.ID, input); err == nil {
				t.Fatal("first rejection should expose the dependency failure")
			}
			if _, err := service.CompleteUpload(ctx, created.Asset.ID, input); !errors.Is(err, ErrInvalidUpload) {
				t.Fatalf("retry error = %v", err)
			}
			if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
				t.Fatal("staging object remains after retry")
			}
			if repo.assets[created.Asset.ID].UploadStatus != UploadFailed {
				t.Fatal("upload was not marked failed")
			}
		})
	}
}

func TestCompleteUploadRecoversFinalWithStagingAfterExpiry(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	repo.completeFailures = 1
	blobs := newMemoryBlobStore()
	blobs.commitLeavesStaging = true
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", func() time.Time { return now })
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-2", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024, Visibility: VisibilityPublic,
	}, "recover-final-and-staging")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nrecover")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	input := CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"}

	if _, err := service.CompleteUpload(ctx, created.Asset.ID, input); err == nil {
		t.Fatal("first completion should expose the database failure")
	}
	now = created.Session.ExpiresAt.Add(time.Hour)
	if _, err := service.CompleteUpload(ctx, created.Asset.ID, input); err != nil {
		t.Fatal(err)
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; ok {
		t.Fatal("recovered completion did not remove staging")
	}
}

func TestCompleteUploadRefetchesAssetForCompletedSession(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-3", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024, Visibility: VisibilityPublic,
	}, "completed-refetch")
	if err != nil {
		t.Fatal(err)
	}
	completed := created.Asset
	completed.SizeBytes = 12
	completed.ChecksumSHA256 = "checksum"
	completed.DetectedMIMEType = "application/pdf"
	completed.UploadStatus = UploadCompleted
	session := created.Session
	session.Status = UploadCompleted
	repo.assets[created.Asset.ID] = completed
	repo.sessions[created.Asset.ID] = session
	stale := created.Asset
	repo.staleAssetOnce = &stale

	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: 12, ChecksumSHA256: "checksum", MIMEType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.UploadStatus != UploadCompleted {
		t.Fatalf("upload status = %q", asset.UploadStatus)
	}
}

func TestCompleteUploadAcceptsConcurrentFinalCommit(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	blobs.commitConflictCreatesFinal = true
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-4", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024, Visibility: VisibilityPublic,
	}, "concurrent-final")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nconcurrent")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)

	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.UploadStatus != UploadCompleted {
		t.Fatalf("upload status = %q", asset.UploadStatus)
	}
}

func TestCreateUploadSessionReplaysIdempotencyKey(t *testing.T) {
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	input := CreateUploadInput{Namespace: "cms.news.cover", OwnerService: "hhc-web-api", OwnerType: "news", OwnerID: "news-1", OriginalFileName: "cover.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 5 << 20, Visibility: VisibilityPublic}

	first, err := service.CreateUploadSession(context.Background(), input, "news-cover-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateUploadSession(context.Background(), input, "news-cover-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Asset.ID != second.Asset.ID || second.Target.URL == "" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestCreateUploadSessionRejectsIdempotencyReplayWithDifferentPurpose(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	input := CreateUploadInput{Namespace: "cms.news.cover", OwnerService: "hhc-web-api", OwnerType: "news", OwnerID: "news-1", Purpose: "cover", OriginalFileName: "cover.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 5 << 20, Visibility: VisibilityPublic}

	if _, err := service.CreateUploadSession(context.Background(), input, "semantic-replay"); err != nil {
		t.Fatal(err)
	}
	input.Purpose = "inline"
	if _, err := service.CreateUploadSession(context.Background(), input, "semantic-replay"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestCreateUploadSessionEnforcesNamespaceOwnerSizeAndVisibility(t *testing.T) {
	service := NewService(newMemoryRepository(), newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	base := CreateUploadInput{Namespace: "account.avatar", OwnerService: "account-api", OwnerType: "user", OwnerID: "user-1", Purpose: "avatar", OriginalFileName: "avatar.jpg", ExpectedMIMEType: "image/jpeg", MaxSizeBytes: 1 << 20, Visibility: VisibilityPublic}

	tests := []CreateUploadInput{
		func() CreateUploadInput { value := base; value.OwnerService = "hhc-web-api"; return value }(),
		func() CreateUploadInput { value := base; value.MaxSizeBytes = (1 << 20) + 1; return value }(),
		func() CreateUploadInput { value := base; value.Visibility = VisibilityPrivate; return value }(),
	}
	for index, input := range tests {
		if _, err := service.CreateUploadSession(context.Background(), input, "invalid-policy-"+string(rune('a'+index))); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d error = %v", index, err)
		}
	}
}

func TestAccountDSRExportUploadAndCompletion(t *testing.T) {
	ctx := context.Background()
	newService := func() (*Service, *memoryRepository, *memoryBlobStore) {
		repo := newMemoryRepository()
		blobs := newMemoryBlobStore()
		return NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now), repo, blobs
	}
	base := CreateUploadInput{
		Namespace: "account.dsr-export", OwnerService: "account-api", OwnerType: "dsr_export", OwnerID: "user-1",
		Purpose: "account_export", OriginalFileName: "export.zip", ExpectedMIMEType: "application/zip",
		MaxSizeBytes: 50 << 20, Visibility: VisibilityPrivate,
	}

	t.Run("accepts ZIP at the 50 MiB policy boundary and stays scan-gated", func(t *testing.T) {
		service, repo, blobs := newService()
		created, err := service.CreateUploadSession(ctx, base, "dsr-valid")
		if err != nil {
			t.Fatal(err)
		}
		payload := testZIP(t, "export.json")
		blobs.objects[created.Session.StagingObjectKey] = payload
		sum := sha256.Sum256(payload)
		asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
			SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/zip",
		})
		if err != nil {
			t.Fatal(err)
		}
		if asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanPending || asset.ProcessingStatus != ProcessingNotRequired || asset.DetectedMIMEType != "application/zip" {
			t.Fatalf("asset = %+v", asset)
		}
		if _, err := service.AuthorizedMetadata(ctx, asset.ID, SubjectService, "account-api"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("pre-clean download metadata error = %v", err)
		}
		if err := service.ApplyScanResult(ctx, ScanResult{EventID: "dsr-scan", AssetID: asset.ID, Status: ScanClean}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectService, SubjectID: "account-api", Permission: PermissionRead, IdempotencyKey: "dsr-grant"}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.AuthorizedMetadata(ctx, asset.ID, SubjectService, "account-api"); err != nil {
			t.Fatalf("clean download metadata error = %v", err)
		}
		if repo.assets[asset.ID].ScanStatus != ScanClean {
			t.Fatalf("scan status = %s", repo.assets[asset.ID].ScanStatus)
		}
	})

	for _, test := range []struct {
		name    string
		input   CreateUploadInput
		payload []byte
	}{
		{name: "wrong MIME", input: func() CreateUploadInput { value := base; value.ExpectedMIMEType = "application/pdf"; return value }(), payload: []byte("%PDF-1.7")},
		{name: "wrong extension", input: func() CreateUploadInput { value := base; value.OriginalFileName = "export.pdf"; return value }(), payload: testZIP(t, "export.json")},
		{name: "non-ZIP bytes", input: base, payload: []byte("not a ZIP")},
		{name: "malformed ZIP", input: base, payload: []byte("PK\x03\x04malformed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, blobs := newService()
			created, err := service.CreateUploadSession(ctx, test.input, "dsr-invalid-"+test.name)
			if test.name == "wrong MIME" || test.name == "wrong extension" {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("create error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			blobs.objects[created.Session.StagingObjectKey] = test.payload
			sum := sha256.Sum256(test.payload)
			if _, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
				SizeBytes: int64(len(test.payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/zip",
			}); !errors.Is(err, ErrInvalidUpload) {
				t.Fatalf("complete error = %v", err)
			}
		})
	}

	for _, test := range []struct {
		name    string
		maxSize int64
		wantErr bool
	}{
		{name: "exact boundary", maxSize: 50 << 20},
		{name: "over boundary", maxSize: (50 << 20) + 1, wantErr: true},
	} {
		input := base
		input.MaxSizeBytes = test.maxSize
		t.Run("max-size-"+test.name, func(t *testing.T) {
			service, _, _ := newService()
			_, err := service.CreateUploadSession(ctx, input, "dsr-size-"+test.name)
			if test.wantErr && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("create error = %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateUploadSessionEnforcesLinePerTypeLimit(t *testing.T) {
	service := NewService(newMemoryRepository(), newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	_, err := service.CreateUploadSession(context.Background(), CreateUploadInput{
		Namespace: "line.group.file", OwnerService: "hhc-line-function-bot", OwnerType: "line_message",
		OwnerID: "message-1", Purpose: "resource", OriginalFileName: "notes.txt",
		ExpectedMIMEType: "text/plain", MaxSizeBytes: (2 << 20) + 1, Visibility: VisibilityRestricted,
	}, "line-text-too-large")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteUploadAcceptsKnownOfficeContainer(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	mime := "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "line.group.file", OwnerService: "hhc-line-function-bot", OwnerType: "line_message",
		OwnerID: "message-1", Purpose: "resource", OriginalFileName: "slides.pptx",
		ExpectedMIMEType: mime, MaxSizeBytes: 1024, Visibility: VisibilityRestricted,
	}, "line-pptx")
	if err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	writer := zip.NewWriter(&zipped)
	contentTypes, err := writer.Create("[Content_Types].xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = contentTypes.Write([]byte("<Types/>"))
	part, err := writer.Create("ppt/presentation.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("<presentation/>"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	payload := zipped.Bytes()
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: mime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.DetectedMIMEType != mime {
		t.Fatalf("detected MIME = %q", asset.DetectedMIMEType)
	}
}

func TestCompleteUploadUsesCanonicalMediaValidation(t *testing.T) {
	tests := []struct {
		name, fileName, mime string
		payload              []byte
		wantErr              bool
	}{
		{name: "LPDeck", fileName: "service.lpdeck", mime: "application/vnd.hhc.presenter+json", payload: []byte(" \n{\"slides\":[]}")},
		{name: "LPDeck malformed", fileName: "service.lpdeck", mime: "application/vnd.hhc.presenter+json", payload: []byte("{not-json"), wantErr: true},
		{name: "LPDeck trailing", fileName: "service.lpdeck", mime: "application/vnd.hhc.presenter+json", payload: []byte("{\"slides\":[]} {\"second\":true}"), wantErr: true},
		{name: "MP4", fileName: "service.mp4", mime: "video/mp4", payload: bmff("isom")},
		{name: "HEIC spoof", fileName: "service.mp4", mime: "video/mp4", payload: bmff("heic"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newMemoryRepository()
			blobs := newMemoryBlobStore()
			service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
			created, err := service.CreateUploadSession(ctx, CreateUploadInput{
				Namespace: "line.group.media-sync", OwnerService: "hhc-line-function-bot", OwnerType: "line_group",
				OwnerID: "group-1", Purpose: "media-sync", OriginalFileName: test.fileName,
				ExpectedMIMEType: test.mime, MaxSizeBytes: int64(len(test.payload)), Visibility: VisibilityRestricted,
			}, "media-"+test.name)
			if err != nil {
				t.Fatal(err)
			}
			blobs.objects[created.Session.StagingObjectKey] = test.payload
			sum := sha256.Sum256(test.payload)
			asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{SizeBytes: int64(len(test.payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: test.mime})
			if test.mime == "video/mp4" && blobs.lastOpenRange.Count == 0 {
				t.Fatal("exact media validation did not use a bounded header range")
			}
			if test.mime == "application/vnd.hhc.presenter+json" && blobs.lastOpenRange.Count != 0 {
				t.Fatal("LPDeck validation did not request the bounded full stream")
			}
			if test.wantErr {
				if !errors.Is(err, ErrInvalidUpload) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || asset.DetectedMIMEType != test.mime {
				t.Fatalf("asset=%+v err=%v", asset, err)
			}
		})
	}
}

func TestCompleteUploadCancellationDoesNotFailOrDeleteUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	payload := testZIP(t, "[Content_Types].xml", "ppt/presentation.xml")
	mime := "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "line.group.media-sync", OwnerService: "hhc-line-function-bot", OwnerType: "line_group",
		OwnerID: "group-1", Purpose: "media-sync", OriginalFileName: "cancel.pptx",
		ExpectedMIMEType: mime, MaxSizeBytes: int64(len(payload)), Visibility: VisibilityRestricted,
	}, "media-cancel")
	if err != nil {
		t.Fatal(err)
	}
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	cancel()
	_, err = service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: mime,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if repo.assets[created.Asset.ID].UploadStatus != UploadCreated || repo.sessions[created.Asset.ID].Status != UploadCreated {
		t.Fatal("cancellation permanently failed the upload")
	}
	if _, ok := blobs.objects[created.Session.StagingObjectKey]; !ok {
		t.Fatal("cancellation deleted the staging object")
	}
}

func TestCompleteUploadRejectsMIMEAndSizeSpoofing(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-2", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 100, Visibility: VisibilityPrivate,
	}, "request-2")
	if err != nil {
		t.Fatal(err)
	}
	blobs.objects[created.Session.StagingObjectKey] = []byte("<html>not a pdf</html>")

	_, err = service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: 1, ChecksumSHA256: "invalid", MIMEType: "application/pdf",
	})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("expected invalid upload, got %v", err)
	}
}

func TestCleanScanAndPublicGrantEnableStableDownload(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	if err := service.ApplyScanResult(ctx, ScanResult{EventID: "scan-1", AssetID: asset.ID, Status: ScanClean}); err != nil {
		t.Fatal(err)
	}
	grant, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{
		SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "grant-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.ID == "" {
		t.Fatal("missing grant id")
	}
	download, err := service.OpenPublic(ctx, asset.ID, ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	body, _ := io.ReadAll(download.Body)
	if !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatalf("unexpected body %q", body)
	}
	if got := service.PublicURL(asset.ID); got != "https://www.alive.org.tw/assets/"+asset.ID {
		t.Fatalf("public url = %s", got)
	}
}

func TestRestrictedDownloadRequiresMatchingGrant(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := Asset{
		ID: "line-file", Namespace: "line.group.file", OwnerService: "hhc-line-function-bot",
		ObjectKey: "assets/line-file/original", UploadStatus: UploadCompleted, ScanStatus: ScanClean,
		ProcessingStatus: ProcessingNotRequired, Visibility: VisibilityRestricted,
		DetectedMIMEType: "application/pdf", SizeBytes: 4, ETag: "etag",
	}
	repo.assets[asset.ID] = asset
	blobs.objects[asset.ObjectKey] = []byte("%PDF")
	if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{
		SubjectType: SubjectLineGroup, SubjectID: "group-1", Permission: PermissionRead, IdempotencyKey: "line-read",
	}); err != nil {
		t.Fatal(err)
	}

	metadata, err := service.AuthorizedMetadata(ctx, asset.ID, SubjectLineGroup, "group-1")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CacheControl != "private, no-store" {
		t.Fatalf("cache control = %q", metadata.CacheControl)
	}
	if _, err := service.AuthorizedMetadata(ctx, asset.ID, SubjectLineGroup, "group-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong subject error = %v", err)
	}
	if _, err := service.AuthorizedMetadata(ctx, asset.ID, SubjectPublic, "*"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("public subject error = %v", err)
	}
}

func TestGrantRejectsUnknownSubjectAndPermission(t *testing.T) {
	service := NewService(newMemoryRepository(), newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	for _, input := range []CreateGrantInput{
		{SubjectType: "unknown", SubjectID: "id", Permission: PermissionRead, IdempotencyKey: "unknown-subject"},
		{SubjectType: SubjectUser, SubjectID: "id", Permission: "write", IdempotencyKey: "unknown-permission"},
	} {
		if _, err := service.CreateGrant(context.Background(), "asset", input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("input=%+v err=%v", input, err)
		}
	}
}

func TestPublicGrantAlsoProtectsStableDerivative(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := Asset{ID: "image-1", ObjectKey: "assets/image-1/original", UploadStatus: UploadCompleted, ScanStatus: ScanClean, ProcessingStatus: ProcessingReady, Visibility: VisibilityPublic, DetectedMIMEType: "image/png", SizeBytes: 8}
	repo.assets[asset.ID] = asset
	repo.derivatives[asset.ID+":large"] = Derivative{AssetID: asset.ID, Variant: "large", ObjectKey: "assets/image-1/derivatives/large.jpg", MIMEType: "image/jpeg", SizeBytes: 10}
	blobs.objects["assets/image-1/derivatives/large.jpg"] = []byte("jpeg-bytes")

	if _, err := service.OpenPublicVariant(ctx, asset.ID, "large", ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("derivative was public without grant: %v", err)
	}
	if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "image-grant"}); err != nil {
		t.Fatal(err)
	}
	download, err := service.OpenPublicVariant(ctx, asset.ID, "large", ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	if download.ContentType != "image/jpeg" {
		t.Fatalf("content type=%s", download.ContentType)
	}
}

func TestInfectedScanDeniesDownloadAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	_, _ = service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "grant-2"})
	result := ScanResult{EventID: "scan-infected", AssetID: asset.ID, Status: ScanInfected, Details: "Malware detected"}
	if err := service.ApplyScanResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyScanResult(ctx, result); err != nil {
		t.Fatalf("duplicate event should be idempotent: %v", err)
	}
	if _, err := service.OpenPublic(ctx, asset.ID, ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("infected asset was downloadable: %v", err)
	}
}

func TestScanResultRejectsChangedCommittedETag(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	originalETag := asset.ETag
	asset.ETag = "mutated"
	repo.assets[asset.ID] = asset

	err := service.ApplyScanResult(ctx, ScanResult{EventID: "stale-scan", AssetID: asset.ID, Status: ScanClean, ETag: originalETag})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("scan error = %v", err)
	}
}

func TestPublicGrantRequiresCleanAsset(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	_, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "pending-public"})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("pending grant error = %v", err)
	}
	if err := service.ApplyScanResult(ctx, ScanResult{EventID: "clean-grant", AssetID: asset.ID, Status: ScanClean}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "clean-public"}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDownloadUsesNamespaceCachePolicy(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	weekly := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	if err := service.ApplyScanResult(ctx, ScanResult{EventID: "cache-weekly", AssetID: weekly.ID, Status: ScanClean}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateGrant(ctx, weekly.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "cache-weekly-grant"}); err != nil {
		t.Fatal(err)
	}
	download, err := service.OpenPublic(ctx, weekly.ID, ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	_ = download.Body.Close()
	if download.CacheControl != "public, max-age=31536000, immutable" {
		t.Fatalf("weekly cache = %q", download.CacheControl)
	}

	avatar := Asset{ID: "avatar-1", Namespace: "account.avatar", OwnerService: "account-api", ObjectKey: "assets/avatar-1/original", UploadStatus: UploadCompleted, ScanStatus: ScanClean, ProcessingStatus: ProcessingNotRequired, Visibility: VisibilityPublic, DetectedMIMEType: "image/jpeg", SizeBytes: 4, ETag: "etag-avatar"}
	repo.assets[avatar.ID] = avatar
	blobs.objects[avatar.ObjectKey] = []byte("jpeg")
	properties, _ := blobs.Inspect(ctx, avatar.ObjectKey, "", 0)
	avatar.ETag = properties.ETag
	repo.assets[avatar.ID] = avatar
	if _, err := service.CreateGrant(ctx, avatar.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "cache-avatar-grant"}); err != nil {
		t.Fatal(err)
	}
	download, err = service.OpenPublic(ctx, avatar.ID, ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	_ = download.Body.Close()
	if download.CacheControl != "public, max-age=31536000, immutable" {
		t.Fatalf("avatar cache = %q", download.CacheControl)
	}
}

func TestSoftDeleteImmediatelyBlocksPublicDownload(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	asset := completedAsset(t, ctx, service, blobs, VisibilityPublic)
	if err := service.ApplyScanResult(ctx, ScanResult{EventID: "delete-clean", AssetID: asset.ID, Status: ScanClean}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateGrant(ctx, asset.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "delete-grant"}); err != nil {
		t.Fatal(err)
	}
	if err := service.SoftDelete(ctx, asset.ID, "hhc-web-api"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.OpenPublic(ctx, asset.ID, ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted download error = %v", err)
	}
	if err := service.SoftDelete(ctx, asset.ID, "hhc-web-api"); err != nil {
		t.Fatalf("soft delete is not idempotent: %v", err)
	}
}

func TestRequeueScanAllowsFailedButNotInfected(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	service := NewService(repo, newMemoryBlobStore(), "https://www.alive.org.tw/api/assets", time.Now)
	repo.assets["failed"] = Asset{ID: "failed", OwnerService: "hhc-web-api", ETag: "etag-failed", ScanStatus: ScanFailed}
	repo.assets["infected"] = Asset{ID: "infected", OwnerService: "hhc-web-api", ETag: "etag-infected", ScanStatus: ScanInfected}

	if err := service.RequeueScan(ctx, "failed", "hhc-web-api"); err != nil {
		t.Fatal(err)
	}
	if repo.assets["failed"].ScanStatus != ScanPending {
		t.Fatalf("failed scan status = %q", repo.assets["failed"].ScanStatus)
	}
	if len(repo.scanRequests) != 1 {
		t.Fatalf("scan requests=%+v", repo.scanRequests)
	}
	if err := service.RequeueScan(ctx, "infected", "hhc-web-api"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("infected requeue error = %v", err)
	}
}

func completedAsset(t *testing.T, ctx context.Context, service *Service, blobs *memoryBlobStore, visibility Visibility) Asset {
	t.Helper()
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_version",
		OwnerID: "version-complete", Purpose: "pdf", OriginalFileName: "weekly.pdf",
		ExpectedMIMEType: "application/pdf", MaxSizeBytes: 5 << 20, Visibility: visibility,
	}, "request-complete")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nclean weekly bulletin")
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestCollectionServiceRequiresMutationIdentity(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", func() time.Time {
		return time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	})

	for _, input := range []CreateCollectionInput{
		{Namespace: "namespace", Name: "Media", CallerService: "helper"},
		{Namespace: "namespace", Name: "Media", IdempotencyKey: "key"},
		{Namespace: "", Name: "Media", CallerService: "helper", IdempotencyKey: "key"},
	} {
		if _, err := service.CreateCollection(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("input=%+v err=%v", input, err)
		}
	}
	if repository.createCalls != 0 {
		t.Fatalf("create calls=%d", repository.createCalls)
	}

	created, err := service.CreateCollection(context.Background(), CreateCollectionInput{
		Namespace: "namespace", Name: "Media", CallerService: "helper", IdempotencyKey: "key",
	})
	if err != nil || created.ID != "collection" || repository.createCalls != 1 {
		t.Fatalf("created=%+v calls=%d err=%v", created, repository.createCalls, err)
	}
}

func TestCollectionMutationJSONCannotSetTrustedIdentity(t *testing.T) {
	var input CreateCollectionInput
	if err := json.Unmarshal([]byte(`{"namespace":"namespace","name":"Media","callerService":"attacker","idempotencyKey":"body-key"}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.CallerService != "" || input.IdempotencyKey != "" {
		t.Fatalf("caller=%q key=%q", input.CallerService, input.IdempotencyKey)
	}
}

func TestCollectionSubjectIsACLOnly(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)

	if _, err := service.ListAuthorizedCollections(context.Background(), CollectionSubject{UserID: "user"}, "", 10); err != nil {
		t.Fatalf("authenticated subject err=%v", err)
	}
	if repository.readerCalls != 1 {
		t.Fatalf("reader calls=%d", repository.readerCalls)
	}
	if _, err := service.ListAuthorizedCollections(context.Background(), CollectionSubject{}, "", 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("empty subject err=%v", err)
	}
	if repository.readerCalls != 1 {
		t.Fatalf("reader calls=%d", repository.readerCalls)
	}
}

func TestCollectionItemAndTicketWithoutGlobalRole(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", func() time.Time { return now })
	subject := CollectionSubject{UserID: "user"}

	if _, err := service.GetAuthorizedCollectionItem(context.Background(), "collection", "item", subject); err != nil {
		t.Fatalf("item err=%v", err)
	}
	if _, err := service.IssueCollectionContentTicket(context.Background(), "collection", "item", subject, now.Add(time.Minute)); err != nil {
		t.Fatalf("ticket err=%v", err)
	}
}

func TestCollectionReaderServiceGetsAuthorizedItem(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)
	subject := CollectionSubject{UserID: "user"}

	item, err := service.GetAuthorizedCollectionItem(context.Background(), "collection", "item", subject)
	if err != nil || item.ID != "item" || repository.readerItemCalls != 1 {
		t.Fatalf("item=%+v calls=%d err=%v", item, repository.readerItemCalls, err)
	}
	if repository.readerItemCalls != 1 {
		t.Fatalf("reader item calls=%d", repository.readerItemCalls)
	}
}

func TestRecordSyncReceiptAuthorizesItemAndValidatesBody(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)
	receipt := SyncReceipt{CollectionItemID: "item", ContentVersion: "etag", State: "available-offline", AppVersion: "2.3.9"}
	if err := service.RecordSyncReceipt(context.Background(), receipt, CollectionSubject{UserID: "user"}); err != nil || repository.receiptCalls != 1 {
		t.Fatalf("calls=%d err=%v", repository.receiptCalls, err)
	}
	for _, invalid := range []SyncReceipt{
		{},
		{CollectionItemID: "item", ContentVersion: "etag", State: "queued", AppVersion: "2.3.9"},
		{CollectionItemID: strings.Repeat("i", 256), ContentVersion: "etag", State: "available-offline", AppVersion: "2.3.9"},
		{CollectionItemID: "item", ContentVersion: "etag", State: "available-offline", AppVersion: strings.Repeat("v", 65)},
	} {
		if err := service.RecordSyncReceipt(context.Background(), invalid, CollectionSubject{UserID: "user"}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("receipt=%+v err=%v", invalid, err)
		}
	}
}

func TestCollectionContentTicketUsesOpaqueHashAndBoundedExpiry(t *testing.T) {
	now := time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC)
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", func() time.Time { return now })
	subject := CollectionSubject{UserID: "user", RoleIDs: []string{"018f0000-0000-7000-8000-000000000001"}}

	issued, err := service.IssueCollectionContentTicket(context.Background(), "collection", "item", subject, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if issued.ExpiresAt != now.Add(5*time.Minute) || issued.ETag != `"asset-version"` {
		t.Fatalf("issued=%+v", issued)
	}
	prefix := "/api/assets/content?ticket="
	if !strings.HasPrefix(issued.ContentURL, prefix) {
		t.Fatalf("content URL=%q", issued.ContentURL)
	}
	token := strings.TrimPrefix(issued.ContentURL, prefix)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token bytes=%d err=%v", len(raw), err)
	}
	hash := sha256.Sum256(raw)
	if repository.ticket.TokenHash != hex.EncodeToString(hash[:]) || strings.Contains(repository.ticket.TokenHash, token) {
		t.Fatalf("persisted hash=%q token=%q", repository.ticket.TokenHash, token)
	}
	if repository.ticket.CollectionID != "collection" || repository.ticket.CollectionItemID != "item" || repository.ticket.AssetETag != issued.ETag || repository.ticket.UserID != "user" || !slices.Equal(repository.ticket.RoleIDs, subject.RoleIDs) {
		t.Fatalf("ticket=%+v", repository.ticket)
	}

	short, err := service.IssueCollectionContentTicket(context.Background(), "collection", "item", subject, now.Add(2*time.Minute))
	if err != nil || short.ExpiresAt != now.Add(2*time.Minute) || short.ContentURL == issued.ContentURL {
		t.Fatalf("short=%+v err=%v", short, err)
	}
	if _, err := service.IssueCollectionContentTicket(context.Background(), "collection", "item", subject, now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token error=%v", err)
	}
}

func TestManagedContentTicketsAreBoundedAndDoNotUseReaderAuthorization(t *testing.T) {
	now := time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC)
	availableID := "550e8400e29b41d4a716446655440000"
	unavailableID := "550e8400e29b41d4a716446655440001"
	repository := &collectionServiceRepository{unavailableTicketItems: map[string]bool{unavailableID: true}}
	service := NewService(repository, newMemoryBlobStore(), "", func() time.Time { return now })
	deniedRepository := &collectionServiceRepository{managedCollectionErr: ErrNotFound}
	deniedService := NewService(deniedRepository, newMemoryBlobStore(), "", func() time.Time { return now })
	if _, err := deniedService.IssueManagedContentTickets(context.Background(), "other-collection", "helper", []string{availableID}, time.Minute); !errors.Is(err, ErrNotFound) || deniedRepository.ticket.TokenHash != "" {
		t.Fatalf("cross-owner ticket err=%v ticket=%+v", err, deniedRepository.ticket)
	}
	nonMediaRepository := &collectionServiceRepository{managedCollectionNamespace: "other"}
	nonMediaService := NewService(nonMediaRepository, newMemoryBlobStore(), "", func() time.Time { return now })
	if _, err := nonMediaService.IssueManagedContentTickets(context.Background(), "other-collection", "helper", []string{availableID}, time.Minute); !errors.Is(err, ErrNotFound) || nonMediaRepository.ticket.TokenHash != "" {
		t.Fatalf("cross-namespace ticket err=%v ticket=%+v", err, nonMediaRepository.ticket)
	}

	if _, err := service.IssueManagedContentTickets(context.Background(), "collection", "", []string{availableID}, time.Minute); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank caller err=%v", err)
	}
	batch, err := service.IssueManagedContentTickets(context.Background(), "collection", "helper", []string{availableID, unavailableID}, 10*time.Minute)
	if err != nil || len(batch.Tickets) != 1 || len(batch.UnavailableItemIDs) != 1 || batch.UnavailableItemIDs[0] != unavailableID {
		t.Fatalf("batch=%+v err=%v", batch, err)
	}
	if batch.Tickets[0].ItemID != availableID || batch.Tickets[0].ExpiresAt != now.Add(5*time.Minute) || batch.Tickets[0].ContentURL == "" || repository.ticket.AccessMode != "manager" || repository.ticket.RoleIDs == nil || repository.readerItemCalls != 0 {
		t.Fatalf("ticket=%+v persisted=%+v reader calls=%d", batch.Tickets[0], repository.ticket, repository.readerItemCalls)
	}

	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = fmt.Sprintf("%032x", i+1)
	}
	batch, err = service.IssueManagedContentTickets(context.Background(), "collection", "helper", hundred, time.Minute)
	if err != nil || len(batch.Tickets) != 100 || len(batch.UnavailableItemIDs) != 0 {
		t.Fatalf("100-item batch=%+v err=%v", batch, err)
	}
	if _, err := service.IssueManagedContentTickets(context.Background(), "collection", "helper", append(hundred, "550e8400e29b41d4a716446655440100"), time.Minute); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("101-item batch err=%v", err)
	}
}

func TestCollectionContentTicketLookupRejectsMalformedTokensAndReturnsPinnedMetadata(t *testing.T) {
	now := time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC)
	repository := &collectionServiceRepository{ticketAsset: Asset{
		ID: "asset", ObjectKey: "assets/asset", OriginalFileName: "video.mp4", DetectedMIMEType: "video/mp4",
		SizeBytes: 6, ETag: `"asset-version"`, UpdatedAt: now, UploadStatus: UploadCompleted,
		ScanStatus: ScanClean, ProcessingStatus: ProcessingReady,
	}}
	service := NewService(repository, newMemoryBlobStore(), "", func() time.Time { return now })
	for _, token := range []string{"", "plain-text", base64.RawURLEncoding.EncodeToString(make([]byte, 31))} {
		if _, err := service.ContentTicketMetadata(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("token=%q err=%v", token, err)
		}
	}
	raw := bytes.Repeat([]byte{7}, 32)
	token := base64.RawURLEncoding.EncodeToString(raw)
	metadata, err := service.ContentTicketMetadata(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if repository.ticketLookupHash != hex.EncodeToString(hash[:]) || metadata.ETag != `"asset-version"` || metadata.Size != 6 || metadata.ContentType != "video/mp4" {
		t.Fatalf("hash=%q metadata=%+v", repository.ticketLookupHash, metadata)
	}
}

func TestCollectionServiceSeparatesManagedReads(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)

	if _, err := service.ListManagedCollections(context.Background(), "", "", 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank caller err=%v", err)
	}
	if _, err := service.ListManagedCollections(context.Background(), "helper", "", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetManagedCollection(context.Background(), "collection", "helper"); err != nil {
		t.Fatal(err)
	}
	if repository.managedListCalls != 1 || repository.managedGetCalls != 1 || repository.readerCalls != 0 {
		t.Fatalf("managed list=%d get=%d reader=%d", repository.managedListCalls, repository.managedGetCalls, repository.readerCalls)
	}
}

func TestManagedCollectionItemsAndRetentionServiceValidation(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)

	if _, err := service.ListManagedCollectionItems(context.Background(), "", "helper", "", "", 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank collection err=%v", err)
	}
	if _, err := service.ListManagedCollectionItems(context.Background(), "collection", "", "", "", 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank caller err=%v", err)
	}
	page, err := service.ListManagedCollectionItems(context.Background(), "collection", "helper", "Sunday", "cursor", 25)
	if err != nil || len(page.Items) != 1 || page.Items[0].DisplayName != "Sunday.mp4" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if repository.managedItemCollectionID != "collection" || repository.managedItemCaller != "helper" || repository.managedItemQuery != "Sunday" || repository.managedItemCursor != "cursor" || repository.managedItemLimit != 25 {
		t.Fatalf("managed item input=%+v", repository)
	}
	for _, query := range []string{"bad\x00query", "bad\nquery", strings.Repeat("a", 256)} {
		if _, err := service.ListManagedCollectionItems(context.Background(), "collection", "helper", query, "", 25); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("query=%q err=%v", query, err)
		}
	}
	if _, err := service.ListManagedCollectionItems(context.Background(), "collection", "helper", "主日", "", 25); err != nil || repository.managedItemQuery != "主日" {
		t.Fatalf("unicode query=%q err=%v", repository.managedItemQuery, err)
	}

	for _, retentionDays := range []int{0, 366} {
		if _, err := service.UpdateCollectionRetention(context.Background(), UpdateCollectionRetentionInput{CollectionID: "collection", RetentionDays: retentionDays, CallerService: "helper", IdempotencyKey: "key"}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("retention days=%d err=%v", retentionDays, err)
		}
	}
	for _, retentionDays := range []int{1, 365} {
		collection, err := service.UpdateCollectionRetention(context.Background(), UpdateCollectionRetentionInput{CollectionID: "collection", RetentionDays: retentionDays, CallerService: "helper", IdempotencyKey: "key"})
		if err != nil || collection.RetentionDays != retentionDays {
			t.Fatalf("retention days=%d collection=%+v err=%v", retentionDays, collection, err)
		}
	}
	if repository.managedRetentionCalls != 2 {
		t.Fatalf("retention calls=%d", repository.managedRetentionCalls)
	}
}

func TestRenameCollectionItemServiceValidation(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)
	input := RenameCollectionItemInput{CollectionID: "collection", ItemID: "item", DisplayName: "  renamed.mp4  ", CallerService: "helper", IdempotencyKey: "rename"}

	item, err := service.RenameCollectionItem(context.Background(), input)
	if err != nil || item.DisplayName != "renamed.mp4" || repository.renameItem.DisplayName != "renamed.mp4" {
		t.Fatalf("item=%+v input=%+v err=%v", item, repository.renameItem, err)
	}
	input.DisplayName = strings.Repeat("a", 255)
	if _, err := service.RenameCollectionItem(context.Background(), input); err != nil || repository.renameItem.DisplayName != input.DisplayName {
		t.Fatalf("255-byte display name err=%v input=%+v", err, repository.renameItem)
	}
	for _, displayName := range []string{"", "folder/file.mp4", `folder\\file.mp4`, "bad\x00.mp4", "bad\n.mp4", strings.Repeat("a", 256)} {
		input.DisplayName = displayName
		if _, err := service.RenameCollectionItem(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("displayName=%q err=%v", displayName, err)
		}
	}
}

type collectionServiceRepository struct {
	Repository
	createCalls                                                                     int
	readerCalls                                                                     int
	managedListCalls                                                                int
	managedGetCalls                                                                 int
	managedCollectionErr                                                            error
	managedCollectionNamespace                                                      string
	managedRetentionCalls                                                           int
	readerItemCalls                                                                 int
	receiptCalls                                                                    int
	managedItemCollectionID, managedItemCaller, managedItemQuery, managedItemCursor string
	managedItemLimit                                                                int
	managedItemCalls                                                                int
	renameItem                                                                      RenameCollectionItemInput
	ticket                                                                          ContentTicket
	ticketAsset                                                                     Asset
	ticketLookupHash                                                                string
	unavailableTicketItems                                                          map[string]bool
}

func (r *collectionServiceRepository) RenameCollectionItem(_ context.Context, input RenameCollectionItemInput, _ time.Time) (ManagedCollectionItem, error) {
	r.renameItem = input
	return ManagedCollectionItem{ID: input.ItemID, DisplayName: input.DisplayName}, nil
}

func (r *collectionServiceRepository) GetAuthorizedCollectionItem(_ context.Context, _, itemID string, _ CollectionSubject) (CollectionItem, error) {
	r.readerItemCalls++
	return CollectionItem{ID: itemID, CollectionID: "collection", AssetID: "asset", ETag: `"asset-version"`}, nil
}

func (r *collectionServiceRepository) GetAuthorizedCollectionItemByID(_ context.Context, itemID string, _ CollectionSubject) (CollectionItem, error) {
	r.receiptCalls++
	return CollectionItem{ID: itemID, CollectionID: "collection", AssetID: "asset", ETag: "etag"}, nil
}

func (r *collectionServiceRepository) GetManagedCollectionItem(_ context.Context, collectionID, itemID, callerService string) (CollectionItem, error) {
	r.managedItemCaller = callerService
	if r.unavailableTicketItems[itemID] {
		return CollectionItem{}, ErrNotFound
	}
	return CollectionItem{ID: itemID, CollectionID: collectionID, AssetID: "asset", ETag: `"asset-version"`}, nil
}

func (r *collectionServiceRepository) CreateContentTicket(_ context.Context, ticket ContentTicket, _ time.Time) error {
	r.ticket = ticket
	if r.unavailableTicketItems[ticket.CollectionItemID] {
		return ErrNotFound
	}
	return nil
}

func (r *collectionServiceRepository) RedeemContentTicket(_ context.Context, tokenHash string, _ time.Time) (Asset, error) {
	r.ticketLookupHash = tokenHash
	if r.ticketAsset.ID == "" {
		return Asset{}, ErrNotFound
	}
	return r.ticketAsset, nil
}

func (r *collectionServiceRepository) CreateCollection(_ context.Context, _ CreateCollectionInput, _ time.Time) (Collection, error) {
	r.createCalls++
	return Collection{ID: "collection"}, nil
}

func (r *collectionServiceRepository) ListAuthorizedCollections(_ context.Context, _ CollectionSubject, _ string, _ int) (CollectionPage, error) {
	r.readerCalls++
	return CollectionPage{}, nil
}

func (r *collectionServiceRepository) ListManagedCollections(_ context.Context, _ string, _ string, _ int) (ManagedCollectionPage, error) {
	r.managedListCalls++
	return ManagedCollectionPage{}, nil
}

func (r *collectionServiceRepository) GetManagedCollection(_ context.Context, _, _ string) (ManagedCollection, error) {
	r.managedGetCalls++
	if r.managedCollectionErr != nil {
		return ManagedCollection{}, r.managedCollectionErr
	}
	namespace := r.managedCollectionNamespace
	if namespace == "" {
		namespace = "line.group.media-sync"
	}
	return ManagedCollection{Collection: Collection{Namespace: namespace}}, nil
}

func (r *collectionServiceRepository) ListManagedCollectionItems(_ context.Context, collectionID, callerService, query, cursor string, limit int) (ManagedCollectionItemPage, error) {
	r.managedItemCalls++
	r.managedItemCollectionID, r.managedItemCaller, r.managedItemQuery, r.managedItemCursor, r.managedItemLimit = collectionID, callerService, query, cursor, limit
	return ManagedCollectionItemPage{Items: []ManagedCollectionItem{{ID: "item", DisplayName: "Sunday.mp4"}}}, nil
}

func (r *collectionServiceRepository) UpdateCollectionRetention(_ context.Context, input UpdateCollectionRetentionInput, _ time.Time) (Collection, error) {
	r.managedRetentionCalls++
	return Collection{ID: input.CollectionID, RetentionDays: input.RetentionDays}, nil
}

type memoryRepository struct {
	Repository
	assets             map[string]Asset
	sessions           map[string]UploadSession
	grants             map[string]Grant
	events             map[string]struct{}
	derivatives        map[string]Derivative
	scanRequests       map[string]ScanRequest
	completeFailures   int
	failUploadFailures int
	staleAssetOnce     *Asset
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{assets: map[string]Asset{}, sessions: map[string]UploadSession{}, grants: map[string]Grant{}, events: map[string]struct{}{}, derivatives: map[string]Derivative{}, scanRequests: map[string]ScanRequest{}}
}

func (r *memoryRepository) CreateUpload(_ context.Context, asset Asset, session UploadSession) error {
	r.assets[asset.ID] = asset
	r.sessions[asset.ID] = session
	return nil
}
func (r *memoryRepository) GetAsset(_ context.Context, id string) (Asset, error) {
	if r.staleAssetOnce != nil {
		value := *r.staleAssetOnce
		r.staleAssetOnce = nil
		return value, nil
	}
	value, ok := r.assets[id]
	if !ok {
		return Asset{}, ErrNotFound
	}
	return value, nil
}
func (r *memoryRepository) GetUploadSession(_ context.Context, assetID string) (UploadSession, error) {
	value, ok := r.sessions[assetID]
	if !ok {
		return UploadSession{}, ErrNotFound
	}
	return value, nil
}
func (r *memoryRepository) FindUploadByIdempotency(_ context.Context, caller, operation, key string) (Asset, UploadSession, error) {
	for assetID, session := range r.sessions {
		if session.CallerService == caller && session.Operation == operation && session.IdempotencyKey == key {
			return r.assets[assetID], session, nil
		}
	}
	return Asset{}, UploadSession{}, ErrNotFound
}
func (r *memoryRepository) CompleteUpload(_ context.Context, asset Asset, session UploadSession, request ScanRequest) error {
	if r.completeFailures > 0 {
		r.completeFailures--
		return errors.New("database unavailable")
	}
	r.assets[asset.ID] = asset
	r.sessions[asset.ID] = session
	if _, exists := r.scanRequests[request.EventID]; !exists {
		r.scanRequests[request.EventID] = request
	}
	return nil
}
func (r *memoryRepository) FailUpload(_ context.Context, assetID string, now time.Time) error {
	if r.failUploadFailures > 0 {
		r.failUploadFailures--
		return errors.New("database unavailable")
	}
	asset, ok := r.assets[assetID]
	if !ok {
		return ErrNotFound
	}
	session := r.sessions[assetID]
	if asset.UploadStatus == UploadFailed && session.Status == UploadFailed {
		return nil
	}
	if asset.UploadStatus != UploadCreated || session.Status != UploadCreated {
		return ErrConflict
	}
	asset.UploadStatus = UploadFailed
	asset.UpdatedAt = now
	session.Status = UploadFailed
	r.assets[assetID] = asset
	r.sessions[assetID] = session
	return nil
}
func (r *memoryRepository) CreateGrant(_ context.Context, grant Grant) (Grant, error) {
	for _, value := range r.grants {
		if value.CallerService == grant.CallerService && value.Operation == grant.Operation && value.IdempotencyKey == grant.IdempotencyKey {
			return value, nil
		}
	}
	r.grants[grant.ID] = grant
	return grant, nil
}
func (r *memoryRepository) RevokeGrant(_ context.Context, assetID, grantID string, _ time.Time) error {
	value, ok := r.grants[grantID]
	if !ok || value.AssetID != assetID {
		return ErrNotFound
	}
	value.RevokedAt = time.Now()
	r.grants[grantID] = value
	return nil
}
func (r *memoryRepository) HasActiveGrant(_ context.Context, assetID string, subject SubjectType, subjectID string, permission Permission, now time.Time) (bool, error) {
	for _, grant := range r.grants {
		if grant.AssetID == assetID && grant.SubjectType == subject && grant.SubjectID == subjectID && grant.Permission == permission && grant.RevokedAt.IsZero() && (grant.ExpiresAt.IsZero() || grant.ExpiresAt.After(now)) {
			return true, nil
		}
	}
	return false, nil
}
func (r *memoryRepository) ApplyScanResult(_ context.Context, result ScanResult, now time.Time) (bool, error) {
	if _, ok := r.events[result.EventID]; ok {
		return false, nil
	}
	asset, ok := r.assets[result.AssetID]
	if !ok {
		return false, ErrNotFound
	}
	if asset.ScanStatus != ScanPending || asset.ETag != result.ETag {
		return false, ErrConflict
	}
	r.events[result.EventID] = struct{}{}
	asset.ScanStatus = result.Status
	asset.ScanDetails = result.Details
	asset.UpdatedAt = now
	r.assets[asset.ID] = asset
	return true, nil
}
func (r *memoryRepository) ClaimPendingScan(_ context.Context, _ time.Time, _ time.Duration) (Asset, bool, error) {
	for id, asset := range r.assets {
		if asset.UploadStatus == UploadCompleted && asset.ScanStatus == ScanPending {
			asset.ScanAttempts++
			r.assets[id] = asset
			return asset, true, nil
		}
	}
	return Asset{}, false, nil
}
func (r *memoryRepository) ScheduleScanRetry(_ context.Context, assetID string, _ int, details string, _, now time.Time) error {
	asset, ok := r.assets[assetID]
	if !ok {
		return ErrNotFound
	}
	asset.ScanDetails = details
	asset.UpdatedAt = now
	r.assets[assetID] = asset
	return nil
}
func (r *memoryRepository) SoftDeleteAsset(_ context.Context, assetID, owner string, now time.Time) error {
	asset, ok := r.assets[assetID]
	if !ok || asset.OwnerService != owner {
		return ErrNotFound
	}
	if asset.DeletedAt.IsZero() {
		asset.DeletedAt = now
		r.assets[assetID] = asset
	}
	return nil
}
func (r *memoryRepository) RequeueFailedScan(_ context.Context, assetID, owner string, request ScanRequest, now time.Time) error {
	asset, ok := r.assets[assetID]
	if !ok || asset.OwnerService != owner {
		return ErrNotFound
	}
	if asset.ScanStatus != ScanFailed {
		return ErrInvalidInput
	}
	asset.ScanStatus = ScanPending
	asset.ScanAttempts = 0
	asset.UpdatedAt = now
	r.assets[assetID] = asset
	r.scanRequests[request.EventID] = request
	return nil
}
func (r *memoryRepository) GetDerivative(_ context.Context, assetID, variant string) (Derivative, error) {
	value, ok := r.derivatives[assetID+":"+variant]
	if !ok {
		return Derivative{}, ErrNotFound
	}
	return value, nil
}

type memoryBlobStore struct {
	objects                    map[string][]byte
	inspectCalls               int
	deleteFailures             int
	commitLeavesStaging        bool
	commitConflictCreatesFinal bool
	lastOpenRange              ByteRange
	lastOpenETag               string
	openCalls                  int
	openETagMismatchAt         int
}

func newMemoryBlobStore() *memoryBlobStore { return &memoryBlobStore{objects: map[string][]byte{}} }
func (b *memoryBlobStore) CreateUploadTarget(_ context.Context, objectKey string, _ int64, expiresAt time.Time) (UploadTarget, error) {
	return UploadTarget{URL: "https://upload.example/" + objectKey, Method: "PUT", ExpiresAt: expiresAt, Headers: map[string]string{"x-ms-blob-type": "BlockBlob"}}, nil
}
func (b *memoryBlobStore) InspectProperties(_ context.Context, objectKey string) (BlobMetadata, error) {
	value, ok := b.objects[objectKey]
	if !ok {
		return BlobMetadata{}, ErrNotFound
	}
	properties := inspectBytes(value)
	return BlobMetadata{Size: properties.Size, ContentType: "application/octet-stream", ETag: "etag-" + properties.ChecksumSHA256}, nil
}
func (b *memoryBlobStore) Inspect(_ context.Context, objectKey, expectedETag string, maxSize int64) (BlobProperties, error) {
	b.inspectCalls++
	value, ok := b.objects[objectKey]
	if !ok {
		return BlobProperties{}, ErrNotFound
	}
	properties := inspectBytes(value)
	properties.ETag = "etag-" + properties.ChecksumSHA256
	if (expectedETag != "" && expectedETag != properties.ETag) || (maxSize > 0 && properties.Size > maxSize) {
		return BlobProperties{}, ErrInvalidUpload
	}
	return properties, nil
}
func (b *memoryBlobStore) Commit(ctx context.Context, stagingObjectKey, finalObjectKey string) (BlobProperties, error) {
	value, ok := b.objects[stagingObjectKey]
	if !ok {
		if _, finalExists := b.objects[finalObjectKey]; finalExists {
			return b.Inspect(ctx, finalObjectKey, "", 0)
		}
		return BlobProperties{}, ErrNotFound
	}
	if _, exists := b.objects[finalObjectKey]; exists {
		return BlobProperties{}, ErrConflict
	}
	b.objects[finalObjectKey] = append([]byte(nil), value...)
	if b.commitConflictCreatesFinal {
		return BlobProperties{}, ErrConflict
	}
	if !b.commitLeavesStaging {
		delete(b.objects, stagingObjectKey)
	}
	return b.Inspect(ctx, finalObjectKey, "", 0)
}
func (b *memoryBlobStore) Open(ctx context.Context, objectKey string, requested ByteRange, expectedETag string) (BlobDownload, error) {
	b.openCalls++
	b.lastOpenRange = requested
	b.lastOpenETag = expectedETag
	if b.openCalls == b.openETagMismatchAt {
		return BlobDownload{}, ErrInvalidUpload
	}
	value, ok := b.objects[objectKey]
	if !ok {
		return BlobDownload{}, ErrNotFound
	}
	properties, _ := b.Inspect(ctx, objectKey, "", 0)
	if expectedETag != "" && properties.ETag != expectedETag {
		return BlobDownload{}, ErrInvalidUpload
	}
	return BlobDownload{Body: io.NopCloser(bytes.NewReader(value)), Size: int64(len(value)), ContentType: "application/pdf", ETag: properties.ETag}, nil
}
func (b *memoryBlobStore) Delete(_ context.Context, objectKey string) error {
	if b.deleteFailures > 0 {
		b.deleteFailures--
		return errors.New("blob unavailable")
	}
	delete(b.objects, objectKey)
	return nil
}
func (b *memoryBlobStore) Put(_ context.Context, objectKey string, reader io.Reader, size int64, _ string) (BlobProperties, error) {
	value, err := io.ReadAll(reader)
	if err != nil || int64(len(value)) != size {
		return BlobProperties{}, ErrInvalidUpload
	}
	b.objects[objectKey] = value
	properties := inspectBytes(value)
	properties.ETag = "etag-derivative"
	return properties, nil
}

func TestCompleteUploadLINETranscodedAudioStaysScanGated(t *testing.T) {
	ctx := context.Background()
	repo := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repo, blobs, "https://www.alive.org.tw/api/assets", time.Now)
	// Synthetic two-second WAV tone sent through LINE; returned as AAC in generic ISO BMFF.
	payload, err := os.ReadFile("testdata/line-transcoded-tone.m4a")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateUploadSession(ctx, CreateUploadInput{
		Namespace: "line.group.media-sync", OwnerService: "hhc-line-function-bot", OwnerType: "media_sync_ingest",
		OwnerID: "test-audio", Purpose: "media-sync", OriginalFileName: "audio.m4a",
		ExpectedMIMEType: "audio/mp4", MaxSizeBytes: int64(len(payload)), Visibility: VisibilityRestricted,
	}, "line-audio")
	if err != nil {
		t.Fatal(err)
	}
	blobs.objects[created.Session.StagingObjectKey] = payload
	sum := sha256.Sum256(payload)
	asset, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "audio/mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanPending || asset.DetectedMIMEType != "audio/mp4" {
		t.Fatalf("unexpected completed audio: %+v", asset)
	}
	if _, err := service.OpenPublic(ctx, asset.ID, ByteRange{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download before clean scan: %v", err)
	}
}
