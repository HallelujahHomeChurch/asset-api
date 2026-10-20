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

func TestRecordingHealthExcludesCaptureProtectedCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	protected := sealedLiveCapture(t, NewRecordingCaptureStore(db), 1)
	eligible := testRecordingPackage(t, "health-cleanup-eligible", "health-actor", "health-recording")
	if err := packages.Create(ctx, eligible); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='failed',cleanup_after=clock_timestamp()-interval '2 hours' WHERE id IN ($1,$2)`, protected, eligible.ID); err != nil {
		t.Fatal(err)
	}
	set := func(query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, protected); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want int) {
		t.Helper()
		ids, err := packages.PackageCleanupCandidates(ctx)
		if err != nil || len(ids) != want || want == 1 && ids[0] != eligible.ID {
			t.Fatalf("cleanup candidates: %v %v; want %d", ids, err, want)
		}
		health, err := RecordingHealth(ctx, db)
		if err != nil || health.PackageCleanupOverdue != int64(want) {
			t.Fatalf("cleanup health disagrees with safe candidates: %+v %v; want %d", health, err, want)
		}
	}
	set(`UPDATE recording_captures SET state='failed',created_at=now()-interval '10 days',expires_at=now()-interval '9 days',terminal_at=now()-interval '6 days',terminal_reason='package_failed' WHERE id=$1`)
	check(1)
	// Expired retention alone cannot override a read grant or either live lease.
	set(`UPDATE recording_live SET read_grant_until=clock_timestamp()+interval '1 hour' WHERE capture_id=$1`)
	set(`UPDATE recording_captures SET terminal_at=clock_timestamp()-interval '8 days' WHERE id=$1`)
	check(1)
	set(`UPDATE recording_live SET read_grant_until=clock_timestamp()-interval '1 second',claim_id='health-live-claim',claimed_until=clock_timestamp()+interval '1 hour' WHERE capture_id=$1`)
	check(1)
	set(`UPDATE recording_processing_slots SET job_id=$1,claim_id='health-slot-claim',leased_until=clock_timestamp()+interval '1 hour' WHERE slot=1`)
	set(`UPDATE recording_live SET claim_id=NULL,claimed_until=NULL WHERE capture_id=$1`)
	check(1)
	set(`UPDATE recording_processing_slots SET leased_until=clock_timestamp()-interval '1 second' WHERE job_id=$1`)
	check(2)
}
