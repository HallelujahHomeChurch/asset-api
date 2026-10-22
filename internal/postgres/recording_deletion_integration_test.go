package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestRecordingDeletionFencesMediaAndCleansReadyPackage(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "delete-package", "delete-actor", "delete-recording")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := packages.FinishPackageValidation(ctx, p.ID, c.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	preview, err := packages.ClaimPackagePreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deletion := NewRecordingDeletionStore(db)
	for i := 0; i < 2; i++ {
		if err := deletion.DeleteRecording(ctx, p.RecordingID); err != nil {
			t.Fatal(err)
		}
	}
	if err := packages.HeartbeatPackagePreview(ctx, p.ID, preview.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("preview lease survived: %v", err)
	}
	got, err := packages.Get(ctx, p.ID)
	if err != nil || got.State != "expired" {
		t.Fatalf("package still ready: %+v %v", got, err)
	}
	next := testRecordingPackage(t, "late-package", "late-actor", p.RecordingID)
	if err := packages.Create(ctx, next); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("late creation: %v", err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET media_expires_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := packages.CleanupPackage(ctx, p.ID, func(_ context.Context, k []string) error { keys = append(keys, k...); return nil }); err != nil {
		t.Fatal(err)
	}
	foundMedia, foundPreview := false, false
	for _, k := range keys {
		if strings.Contains(k, "/final/") && strings.HasSuffix(k, "master.m3u8") {
			foundMedia = true
		}
		if strings.HasSuffix(k, "previews/current.json") {
			foundPreview = true
		}
	}
	if !foundMedia || !foundPreview {
		t.Fatalf("incomplete cleanup: %v", keys)
	}
}

func TestRecordingDeletionFencesSourceJobAndRetriesCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, time.Now)
	p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "delete-source-actor", RecordingID: "delete-source-recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "delete-source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(ctx, p.ID, p.ActorID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, p.RecordingID); err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatSourceProcessing(ctx, p.ID, claim.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("lease survived: %v", err)
	}
	inv := testRecordingPackage(t, claim.ClaimID, p.ActorID, p.RecordingID).Inventory
	if err := store.FinishSourceProcessing(ctx, p.ID, claim.ClaimID, inv); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("source resurrected: %v", err)
	}
	_, err = svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: p.ActorID, RecordingID: p.RecordingID, FileName: "late.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("b", 64), IdempotencyKey: "late-source"})
	if !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("late source accepted: %v", err)
	}
	providerFailure := errors.New("cleanup unavailable")
	if err := store.ReconcileSources(ctx, func(context.Context, string, []string) error { return providerFailure }, func(context.Context, string) error { return nil }); !errors.Is(err, providerFailure) {
		t.Fatalf("failure lost: %v", err)
	}
	deleted := false
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET cleanup_after=clock_timestamp()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileSources(ctx, func(_ context.Context, id string, _ []string) error { deleted = id == p.ID; return nil }, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("temporary source not cleaned")
	}
}

func TestRecordingDeletionExpiresReadySourceAndRetainsAttemptReceipt(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, time.Now)
	p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "ready-delete-actor", RecordingID: "ready-delete-recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "ready-delete"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(ctx, p.ID, p.ActorID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	copy := assets.RecordingSourceCopy{Key: "recording-sources/" + p.ID + "/final/" + claim.ClaimID + "/source", State: "success", SizeBytes: 5, ETag: "verified-etag"}
	if err := store.CheckpointSourceCopy(ctx, p.ID, claim.ClaimID, copy); err != nil {
		t.Fatal(err)
	}
	inv := testRecordingPackage(t, p.ID, p.ActorID, p.RecordingID).Inventory
	if err := store.FinishSourceProcessing(ctx, p.ID, claim.ClaimID, inv); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, p.RecordingID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.State != "expired" || got.PackageID != "" || got.SourceKey != copy.Key {
		t.Fatalf("source deletion did not retain input receipt: %+v %v", got, err)
	}
	pack, err := NewRecordingPackageStore(db).Get(ctx, claim.ClaimID)
	if err != nil || pack.State != "expired" {
		t.Fatalf("derived package not fenced: %+v %v", pack, err)
	}
	var attemptState string
	if err := db.QueryRowContext(ctx, `SELECT state FROM recording_source_attempts WHERE source_id=$1 AND claim_id=$2`, p.ID, claim.ClaimID).Scan(&attemptState); err != nil || attemptState != "ready" {
		t.Fatalf("completed attempt receipt lost: %q %v", attemptState, err)
	}
}
