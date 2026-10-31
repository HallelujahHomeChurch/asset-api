package postgres

import (
	"context"
	"hhc/asset-api/internal/assets"
	"strings"
	"testing"
	"time"
)

func TestTrustedBulletinCompletionCommitsWithoutScanOutbox(t *testing.T) {
	db := integrationDB(t)
	store, ctx, now := New(db), context.Background(), time.Now().UTC()
	asset := assets.Asset{ID: strings.Repeat("a", 32), Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_download_job", OwnerID: "job", Purpose: "weekly_bulletin_member", Locale: "zh-Hant", ObjectKey: "test/derived/original", ExpectedMIMEType: "application/pdf", UploadStatus: assets.UploadCreated, ScanStatus: assets.ScanPending, ProcessingStatus: assets.ProcessingNotRequired, Visibility: assets.VisibilityPrivate, CreatedAt: now, UpdatedAt: now}
	session := assets.UploadSession{ID: strings.Repeat("d", 32), AssetID: asset.ID, IdempotencyKey: "derived", CallerService: asset.OwnerService, Operation: "create_upload", StagingObjectKey: "test/derived/staging", MaxSizeBytes: 1024, Status: assets.UploadCreated, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := store.CreateUpload(ctx, asset, session); err != nil {
		t.Fatal(err)
	}
	asset.UploadStatus, asset.ScanStatus, asset.ScanSignature = assets.UploadCompleted, assets.ScanClean, assets.TrustedBulletinWatermark
	asset.ScanDetails, asset.DetectedMIMEType, asset.ChecksumSHA256, asset.ETag, asset.SizeBytes = strings.Repeat("b", 32)+":"+strings.Repeat("c", 64), "application/pdf", strings.Repeat("e", 64), "immutable", 20
	session.Status, session.CompletedAt = assets.UploadCompleted, now
	for range 2 {
		if err := store.CompleteUpload(ctx, asset, session, assets.ScanRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := store.GetAsset(ctx, asset.ID)
	if err != nil || stored.ScanStatus != assets.ScanClean || stored.ScanSignature != assets.TrustedBulletinWatermark || stored.ScanDetails != asset.ScanDetails {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	var queued int
	if err := db.QueryRow(`SELECT count(*) FROM asset_scan_outbox WHERE asset_id=$1`, asset.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
}
