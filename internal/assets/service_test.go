package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
		{name: "LPDeck", fileName: "service.lpdeck", mime: "application/vnd.librepresenter.presentation+json", payload: []byte(" \n{\"slides\":[]}")},
		{name: "LPDeck malformed", fileName: "service.lpdeck", mime: "application/vnd.librepresenter.presentation+json", payload: []byte("{not-json"), wantErr: true},
		{name: "LPDeck trailing", fileName: "service.lpdeck", mime: "application/vnd.librepresenter.presentation+json", payload: []byte("{\"slides\":[]} {\"second\":true}"), wantErr: true},
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
			if test.mime == "application/vnd.librepresenter.presentation+json" && blobs.lastOpenRange.Count != 0 {
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

func TestCollectionServiceRequiresGlobalReaderRole(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)

	for _, subject := range []CollectionSubject{
		{},
		{UserID: "user", Roles: []string{"admin"}},
		{UserID: "", Roles: []string{CollectionReaderRole}},
	} {
		if _, err := service.ListAuthorizedCollections(context.Background(), subject, "", 10); !errors.Is(err, ErrForbidden) {
			t.Fatalf("subject=%+v err=%v", subject, err)
		}
	}
	if repository.readerCalls != 0 {
		t.Fatalf("reader calls=%d", repository.readerCalls)
	}

	if _, err := service.ListAuthorizedCollections(context.Background(), CollectionSubject{
		UserID: "user", Roles: []string{"role", CollectionReaderRole},
	}, "", 10); err != nil {
		t.Fatal(err)
	}
	if repository.readerCalls != 1 {
		t.Fatalf("reader calls=%d", repository.readerCalls)
	}
}

func TestCollectionReaderServiceGetsAuthorizedItem(t *testing.T) {
	repository := &collectionServiceRepository{}
	service := NewService(repository, newMemoryBlobStore(), "", time.Now)
	subject := CollectionSubject{UserID: "user", Roles: []string{CollectionReaderRole}}

	item, err := service.GetAuthorizedCollectionItem(context.Background(), "collection", "item", subject)
	if err != nil || item.ID != "item" || repository.readerItemCalls != 1 {
		t.Fatalf("item=%+v calls=%d err=%v", item, repository.readerItemCalls, err)
	}
	if _, err := service.GetAuthorizedCollectionItem(context.Background(), "collection", "item", CollectionSubject{UserID: "user"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing reader role err=%v", err)
	}
	if repository.readerItemCalls != 1 {
		t.Fatalf("reader item calls=%d", repository.readerItemCalls)
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

type collectionServiceRepository struct {
	Repository
	createCalls      int
	readerCalls      int
	managedListCalls int
	managedGetCalls  int
	readerItemCalls  int
}

func (r *collectionServiceRepository) GetAuthorizedCollectionItem(_ context.Context, _, itemID string, _ CollectionSubject) (CollectionItem, error) {
	r.readerItemCalls++
	return CollectionItem{ID: itemID}, nil
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
	return ManagedCollection{}, nil
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
	b.lastOpenRange = requested
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
