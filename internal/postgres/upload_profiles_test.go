package postgres

import (
	"context"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestUploadProfilePairPersistsAndLegacyRowsStayEmpty(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	insertAsset(t, db, "legacy-profile-fixture", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	legacy, err := store.GetAsset(ctx, "legacy-profile-fixture")
	if err != nil || legacy.UploadProfiles != (assets.UploadProfiles{}) {
		t.Fatalf("legacy policy = %+v, err=%v", legacy.UploadProfiles, err)
	}
	pair := assets.UploadProfiles{SecurityProfile: "web-image-v1", ProcessingProfile: "image-variants-v1"}
	asset := assets.Asset{
		ID: "explicit-profile-fixture", Namespace: "cms.page.hero", OwnerService: "hhc-web-api", OwnerType: "page", OwnerID: "page-1",
		Purpose: "hero", OriginalFileName: "hero.png", ObjectKey: "hero-original", ExpectedMIMEType: "image/png",
		UploadStatus: assets.UploadCreated, ScanStatus: assets.ScanPending, ProcessingStatus: assets.ProcessingPending,
		Visibility: assets.VisibilityPrivate, CreatedAt: now, UpdatedAt: now, UploadProfiles: pair,
	}
	session := assets.UploadSession{
		ID: "session", AssetID: asset.ID, IdempotencyKey: "hero", CallerService: asset.OwnerService, Operation: "create_upload",
		StagingObjectKey: "hero-staging", MaxSizeBytes: 1024, Status: assets.UploadCreated, ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	}
	if err := store.CreateUpload(ctx, asset, session); err != nil {
		t.Fatal(err)
	}
	read, _, err := store.FindUploadByIdempotency(ctx, asset.OwnerService, session.Operation, session.IdempotencyKey)
	if err != nil || read.UploadProfiles != pair {
		t.Fatalf("persisted policy = %+v, err=%v", read.UploadProfiles, err)
	}
	if _, err := db.Exec("UPDATE assets SET processing_profile='' WHERE id=$1", asset.ID); err == nil {
		t.Fatal("database accepted a half profile pair")
	}
}
