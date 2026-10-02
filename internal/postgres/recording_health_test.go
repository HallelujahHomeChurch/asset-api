package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestRecordingHealthSeparatesLiveUploadsAndOverdueCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	if err := store.Create(ctx, testRecordingPackage(t, "package-health", "actor-health", "recording-health")); err != nil {
		t.Fatal(err)
	}
	sources := assets.NewRecordingSourceService(NewRecordingSourceStore(db), &sourceBlobFixture{}, time.Now)
	source, err := sources.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "source-health", RecordingID: "source-health", FileName: "source.mp4", SizeBytes: 50, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "health"})
	if err != nil {
		t.Fatal(err)
	}
	// Uploads still within their retention deadline are not cleanup failures,
	// even when their initial cleanup check is older than an hour.
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=clock_timestamp()-interval '2 hours'; UPDATE recording_sources SET cleanup_after=clock_timestamp()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	health, err := RecordingHealth(ctx, db)
	if err != nil || health.PackageCleanupOverdue != 0 || health.SourceCleanupOverdue != 0 {
		t.Fatalf("live uploads: %+v %v", health, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='expired'; UPDATE recording_sources SET state='expired'`); err != nil {
		t.Fatal(err)
	}
	health, err = RecordingHealth(ctx, db)
	if err != nil || health.PackageCleanupOverdue != 1 || health.SourceCleanupOverdue != 1 {
		t.Fatalf("expired uploads: %+v %v", health, err)
	}
	if err := store.ReconcilePackages(ctx, func(context.Context, []string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := NewRecordingSourceStore(db).ReconcileSources(ctx, func(_ context.Context, id string, _ []string) error {
		if id != source.ID {
			t.Fatalf("unexpected source %s", id)
		}
		return nil
	}, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	health, err = RecordingHealth(ctx, db)
	if err != nil || health.PackageCleanupOverdue != 0 || health.SourceCleanupOverdue != 0 {
		t.Fatalf("swept uploads: %+v %v", health, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='freezing',completed_at=clock_timestamp(); UPDATE recording_processing_slots SET job_id='package-health',claim_id='claim-health',leased_until=clock_timestamp()+interval '1 minute' WHERE slot=1`); err != nil {
		t.Fatal(err)
	}
	health, err = RecordingHealth(ctx, db)
	if err != nil || health.Waiting != 1 || health.ActiveSlots != 1 {
		t.Fatalf("queued and leased: %+v %v", health, err)
	}
}
