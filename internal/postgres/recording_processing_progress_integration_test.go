package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestRecordingProcessingProgressFenced(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "progress-package", "progress-actor", "progress-recording")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	first, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func(table, id string) assets.RecordingProcessingProgress {
		t.Helper()
		var data []byte
		if err := db.QueryRowContext(ctx, "SELECT processing_progress FROM "+table+" WHERE id=$1", id).Scan(&data); err != nil {
			t.Fatal(err)
		}
		var value assets.RecordingProcessingProgress
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	value := read("recording_packages", p.ID)
	if value.Attempt != 1 || value.Phase != "package_validation" {
		t.Fatalf("initial progress: %+v", value)
	}
	one := int64(1)
	value.ObjectsVerified = &one
	value.ObjectsTotal = &one
	value.LastProgressAt = time.Now().UTC()
	value.HeartbeatAt = value.LastProgressAt
	if err := packages.UpdatePackageProcessingProgress(ctx, p.ID, first.ClaimID, value); err != nil {
		t.Fatal(err)
	}
	if err := packages.HeartbeatPackageValidation(ctx, p.ID, first.ClaimID); err != nil {
		t.Fatal(err)
	}
	after := read("recording_packages", p.ID)
	if !after.LastProgressAt.Equal(value.LastProgressAt) || !after.AttemptStartedAt.Equal(value.AttemptStartedAt) || after.HeartbeatAt.Before(value.HeartbeatAt) {
		t.Fatalf("heartbeat fabricated progress: %+v", after)
	}
	wrong := value
	wrong.AttemptStartedAt = wrong.AttemptStartedAt.Add(-time.Second)
	if err := packages.UpdatePackageProcessingProgress(ctx, p.ID, first.ClaimID, wrong); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("attempt start changed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()-interval '1 second' WHERE claim_id=$1`, first.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := packages.UpdatePackageProcessingProgress(ctx, p.ID, first.ClaimID, value); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("expired progress accepted: %v", err)
	}
	second, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := packages.UpdatePackageProcessingProgress(ctx, p.ID, first.ClaimID, value); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale progress accepted: %v", err)
	}
	reset := read("recording_packages", p.ID)
	if reset.Attempt != 2 || reset.ObjectsVerified != nil || !reset.AttemptStartedAt.After(value.AttemptStartedAt) {
		t.Fatalf("attempt not reset: %+v", reset)
	}
	if err := packages.FinishPackageValidation(ctx, p.ID, second.ClaimID, false, "invalid"); err != nil {
		t.Fatal(err)
	}
	if err := packages.UpdatePackageProcessingProgress(ctx, p.ID, second.ClaimID, reset); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("terminal progress accepted: %v", err)
	}
	// A pre-upgrade worker can finish without ever writing a summary.
	legacy := testRecordingPackage(t, "legacy-progress", "legacy-actor", "legacy-recording")
	if err := packages.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, legacy.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	old, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET processing_progress=NULL WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if err := packages.HeartbeatPackageValidation(ctx, legacy.ID, old.ClaimID); err != nil {
		t.Fatal(err)
	}
	if err := packages.FinishPackageValidation(ctx, legacy.ID, old.ClaimID, false, "invalid"); err != nil {
		t.Fatal(err)
	}

	sources := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(sources, &sourceBlobFixture{}, time.Now)
	source, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "source-progress-actor", RecordingID: "source-progress-recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "source-progress"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(ctx, source.ID, source.ActorID); err != nil {
		t.Fatal(err)
	}
	sourceClaim, err := sources.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := read("recording_sources", source.ID)
	if snapshot.Phase != "source_finalization" || snapshot.Attempt != 1 {
		t.Fatalf("source phase: %+v", snapshot)
	}
	if err := sources.UpdateSourceProcessingProgress(ctx, source.ID, "wrong-claim", snapshot); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale source progress accepted: %v", err)
	}
	if err := sources.UpdateSourceProcessingProgress(ctx, source.ID, sourceClaim.ClaimID, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := sources.HeartbeatSourceProcessing(ctx, source.ID, sourceClaim.ClaimID); err != nil {
		t.Fatal(err)
	}
	if got := read("recording_sources", source.ID); !got.LastProgressAt.Equal(snapshot.LastProgressAt) {
		t.Fatal("source heartbeat fabricated progress")
	}
}
