package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestProfileSelectedAtUploadSurvivesCompletionGrantAndPublicRead(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repository, blobs, "", time.Now)
	profiles := UploadProfiles{SecurityProfile: "web-image-v1", ProcessingProfile: "image-variants-v1"}
	input := CreateUploadInput{
		Namespace: "cms.page.hero", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "page-1", Purpose: "hero",
		ExpectedMIMEType: "image/png", OriginalFileName: "hero.png", MaxSizeBytes: 1024, Visibility: VisibilityPublic,
		UploadProfiles: profiles,
	}
	created, err := service.CreateUploadSession(ctx, input, "hero-upload")
	if err != nil {
		t.Fatal(err)
	}
	if created.Asset.UploadProfiles != profiles || created.Asset.ProcessingStatus != ProcessingPending {
		t.Fatalf("created asset policy = %+v", created.Asset)
	}
	payload := encodePNG(t, 1, 1)
	blobs.objects[created.Session.StagingObjectKey] = payload
	digest := sha256.Sum256(payload)
	completed, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(digest[:]), MIMEType: "image/png",
	})
	if err != nil || completed.ScanStatus != ScanPending || completed.UploadProfiles != profiles {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	completed.ScanStatus = ScanClean
	completed.ProcessingStatus = ProcessingReady
	repository.assets[completed.ID] = completed
	_, err = service.CreateGrant(ctx, completed.ID, CreateGrantInput{SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "hero-publish"})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.PublicMetadata(ctx, completed.ID, "")
	if err != nil || metadata.ContentType != "image/png" || metadata.ETag != completed.ETag || metadata.CacheControl != "public, max-age=31536000, immutable" {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}
}

func TestProfileLegacyOwnerFamiliesDoNotExpandCleanupScope(t *testing.T) {
	for _, namespace := range []string{"account.avatar", "line.group.file", "line.group.media-sync", PersonalNamespace} {
		pair := legacyUploadProfiles[namespace]
		policy, _ := PolicyFor(namespace)
		scope := UploadProfileScope{NamespaceFamily: namespace, UploadProfiles: pair}
		if !scope.ValidForCaller(policy.OwnerService) {
			t.Fatalf("existing %s profile scope rejected", namespace)
		}
		if _, ok := UploadPolicy(namespace+".new", pair); ok {
			t.Fatalf("unmapped %s accepted outside its existing cleanup or retention scope", namespace)
		}
	}
}

func TestOmittedUploadProfilesRetainLegacyFingerprint(t *testing.T) {
	input := CreateUploadInput{
		Namespace: "cms.page.image", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "page-1",
		Purpose: "page_image", OriginalFileName: "image.png", ExpectedMIMEType: "image/png",
		MaxSizeBytes: 1024, Visibility: VisibilityPrivate,
	}
	const legacy = "c34083af3ab3951ad09c9c6278194110315018598edd00f60e895b787b321b3f"
	if got := requestFingerprint(input); got != legacy {
		t.Fatalf("legacy upload fingerprint changed: %s", got)
	}
}

func TestDelegatedBannerCannotPublishWrongDimensionsAfterRestoringAssetGuard(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	blobs := newMemoryBlobStore()
	service := NewService(repository, blobs, "", time.Now).WithHomeBannerDimensionsDelegated(true)
	input := CreateUploadInput{
		Namespace: "cms.home.banner", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "page-1",
		Purpose: "home_banner", ExpectedMIMEType: "image/jpeg", OriginalFileName: "banner.jpg",
		MaxSizeBytes: 10 << 20, Visibility: VisibilityPublic, UploadProfiles: legacyUploadProfiles["cms.home.banner"],
	}
	created, err := service.CreateUploadSession(ctx, input, "delegated-banner")
	if err != nil {
		t.Fatal(err)
	}
	payload := encodeJPEG(t, 1899, 700, false)
	blobs.objects[created.Session.StagingObjectKey] = payload
	digest := sha256.Sum256(payload)
	completed, err := service.CompleteUpload(ctx, created.Asset.ID, CompleteUploadInput{
		SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(digest[:]), MIMEType: "image/jpeg",
	})
	if err != nil {
		t.Fatal(err)
	}
	completed.ScanStatus = ScanClean
	repository.assets[completed.ID] = completed
	restored := NewService(repository, blobs, "", time.Now)
	_, err = restored.CreateGrant(ctx, completed.ID, CreateGrantInput{
		SubjectType: SubjectPublic, SubjectID: "*", Permission: PermissionRead, IdempotencyKey: "old-cms-publish",
	})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("old CMS publication after restoring Asset guard: %v", err)
	}
	if _, err := restored.PublicMetadata(ctx, completed.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unvalidated banner has public metadata: %v", err)
	}
}
