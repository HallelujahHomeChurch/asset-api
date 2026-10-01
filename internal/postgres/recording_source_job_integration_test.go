package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestSourceJobsShareGlobalSlotsAndFenceStaleWorkers(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, time.Now)
	for i := 0; i < 3; i++ {
		p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: fmt.Sprintf("actor-%d", i), RecordingID: fmt.Sprintf("recording-%d", i), FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: fmt.Sprintf("source-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Complete(ctx, p.ID, p.ActorID); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.ID == second.Source.ID {
		t.Fatal("same source claimed twice")
	}
	if _, err := store.ClaimSourceProcessing(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("third source got slot: %v", err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "cli-package", "cli-actor", "cli-recording")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("package ignored shared slots: %v", err)
	}
	if err := store.HeartbeatSourceProcessing(ctx, first.Source.ID, first.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()-interval '1 second' WHERE claim_id=$1`, first.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, first.Source.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimSourceProcessing(ctx)
	if err != nil || reclaimed.Source.ID != first.Source.ID || reclaimed.ClaimID == first.ClaimID || reclaimed.Source.CopyAttemptID != first.ClaimID {
		t.Fatalf("copy recovery: %+v %v", reclaimed, err)
	}
	copy := assets.RecordingSourceCopy{Key: "recording-sources/" + first.Source.ID + "/final/" + first.ClaimID + "/source", State: "success", SizeBytes: 5, ETag: "verified-etag"}
	if err := store.CheckpointSourceCopy(ctx, first.Source.ID, first.ClaimID, copy); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	if err := store.HeartbeatSourceProcessing(ctx, first.Source.ID, first.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if err := store.CheckpointSourceCopy(ctx, reclaimed.Source.ID, reclaimed.ClaimID, copy); err != nil {
		t.Fatal(err)
	}
	if err := store.FailSourceProcessing(ctx, reclaimed.Source.ID, reclaimed.ClaimID, "retry"); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, first.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != "failed" || before.SourceKey != copy.Key {
		t.Fatalf("lost retained source: %+v", before)
	}
	if err := store.Retry(ctx, before.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimSourceProcessing(ctx)
	if err != nil || again.Source.ID != before.ID || again.Source.SourceKey != copy.Key || !again.Source.RetryUntil.Equal(*before.RetryUntil) {
		t.Fatalf("retained-source retry: %+v %v", again, err)
	}
	inv := testRecordingPackage(t, again.Source.ID, again.Source.ActorID, again.Source.RecordingID).Inventory
	if err := store.FinishSourceProcessing(ctx, again.Source.ID, again.ClaimID, inv); err != nil {
		t.Fatal(err)
	}
	ready, err := packages.Get(ctx, again.ClaimID)
	if err != nil || ready.State != "ready" || ready.ReadyAt == nil || ready.MediaExpiresAt == nil || !ready.MediaExpiresAt.Equal(ready.ReadyAt.Add(30*24*time.Hour)) {
		t.Fatalf("ready package: %+v %v", ready, err)
	}
	done, err := store.Get(ctx, again.Source.ID)
	if err != nil || done.State != "ready" || done.PackageID != ready.ID {
		t.Fatalf("source projection: %+v %v", done, err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); err != nil {
		t.Fatalf("source did not release slot: %v", err)
	}
	if err := store.FailSourceProcessing(ctx, second.Source.ID, second.ClaimID, "invalid"); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, second.Source.ID, time.Now()); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("invalid media retried: %v", err)
	}
}

func TestSourceRetryBudgetAndDeadlineCannotBeReset(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, time.Now)
	p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "actor", RecordingID: "recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "source"})
	if err != nil {
		t.Fatal(err)
	}
	p, err = svc.Complete(ctx, p.ID, p.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		claim, err := store.ClaimSourceProcessing(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if claim.Source.ProcessingAttempts != i {
			t.Fatal("attempt counter reset")
		}
		if err := store.FailSourceProcessing(ctx, p.ID, claim.ClaimID, "retry"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Retry(ctx, p.ID, "other"); !errors.Is(err, assets.ErrForbidden) {
			t.Fatalf("cross-owner retry: %v", err)
		}
		if i < 3 {
			if _, err := svc.Retry(ctx, p.ID, p.ActorID); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Retry(ctx, p.ID, p.ActorID); err != nil {
				t.Fatalf("lost retry response: %v", err)
			}
		} else if _, err := svc.Retry(ctx, p.ID, p.ActorID); !errors.Is(err, assets.ErrConflict) {
			t.Fatalf("exhausted source retried: %v", err)
		}
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.FailureCode != "exhausted" || !got.RetryUntil.Equal(*p.RetryUntil) {
		t.Fatalf("budget/deadline %+v %v", got, err)
	}
}
