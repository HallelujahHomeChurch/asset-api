package postgres

import (
	"context"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestLineSourcePersistsDeadlineAndCompletesWithoutScanOutbox(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().In(time.FixedZone("synthetic", 8*3600+30*60)).Truncate(time.Microsecond)
	a := assets.Asset{ID: "new-line-source", Namespace: "line.group.file", OwnerService: assets.LineHarnessCaller, OwnerType: "line_message", OwnerID: "opaque", Purpose: "original", OriginalFileName: "file.pdf", ObjectKey: "source-original", ExpectedMIMEType: "application/pdf", UploadStatus: assets.UploadCreated, ScanStatus: assets.ScanPending, ProcessingStatus: assets.ProcessingNotRequired, Visibility: assets.VisibilityRestricted, CreatedAt: now, UpdatedAt: now, UploadProfiles: assets.UploadProfiles{SecurityProfile: assets.LineSourceSecurityProfile, ProcessingProfile: "no-processing-v1"}, Source: &assets.SourcePolicy{SourceReference: assets.SourceReference{Ref: "source_opaque_123", Revision: "revision_opaque_123", PublishedAt: now}, Version: 1, ExpiresAt: now.Add(assets.SourceLifetime)}}
	u := assets.UploadSession{ID: "source-session", AssetID: a.ID, IdempotencyKey: "source-operation", CallerService: a.OwnerService, Operation: "create_upload", StagingObjectKey: "source-staging", MaxSizeBytes: 1024, Status: assets.UploadCreated, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := store.CreateUpload(ctx, a, u); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAsset(ctx, a.ID)
	if err != nil || got.Source == nil || got.Source.Ref != a.Source.Ref || !got.Source.PublishedAt.Equal(a.Source.PublishedAt) || !got.Source.ExpiresAt.Equal(a.Source.ExpiresAt) {
		t.Fatalf("read source %+v %v", got.Source, err)
	}
	a.UploadStatus = assets.UploadCompleted
	a.ScanStatus = assets.ScanNotRequired
	a.ETag = "immutable-etag"
	a.DetectedMIMEType = "application/pdf"
	a.ChecksumSHA256 = "digest"
	a.SizeBytes = 23
	u.Status = assets.UploadCompleted
	u.CompletedAt = now
	if err := store.CompleteUpload(ctx, a, u, assets.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload(ctx, a, u, assets.ScanRequest{}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM asset_scan_outbox WHERE asset_id=$1`, a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("scan queue %d %v", count, err)
	}
	got, err = store.GetAsset(ctx, a.ID)
	if err != nil || got.ScanStatus != assets.ScanNotRequired || got.Source == nil {
		t.Fatalf("completed %+v %v", got, err)
	}
	for _, statement := range []string{
		`UPDATE assets SET source_expires_at=source_expires_at+interval '1 hour' WHERE id=$1`,
		`UPDATE assets SET scan_status='clean' WHERE id=$1`,
		`UPDATE assets SET owner_service='hhc-line-function-bot' WHERE id=$1`,
		`UPDATE assets SET source_policy=NULL WHERE id=$1`,
	} {
		if _, err := db.Exec(statement, a.ID); err == nil {
			t.Fatalf("database accepted %s", statement)
		}
	}
	a.ETag = "changed"
	if err := store.CompleteUpload(ctx, a, u, assets.ScanRequest{}); err == nil {
		t.Fatal("changed replay accepted")
	}
}
