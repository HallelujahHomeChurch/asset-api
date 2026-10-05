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

func TestSourceCleanupRetainsRetryInputAndNeverDeletesReadyOutput(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, time.Now)
	p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "cleanup-actor", RecordingID: "cleanup-recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "cleanup-source"})
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
	copy := assets.RecordingSourceCopy{Key: "recording-sources/" + p.ID + "/final/" + claim.ClaimID + "/source", State: "success", ETag: "etag", SizeBytes: 5}
	if err := store.CheckpointSourceCopy(ctx, p.ID, claim.ClaimID, copy); err != nil {
		t.Fatal(err)
	}
	if err := store.FailSourceProcessing(ctx, p.ID, claim.ClaimID, "retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_source_attempts SET finished_at=clock_timestamp()-interval '7 hours' WHERE claim_id=$1`, claim.ClaimID); err != nil {
		t.Fatal(err)
	}
	blobDeletes, outputDeletes := 0, 0
	deleteBlob := func(_ context.Context, id string, attempts []string) error {
		blobDeletes++
		if id != p.ID || len(attempts) != 2 {
			t.Fatalf("wrong source cleanup scope %s %v", id, attempts)
		}
		return nil
	}
	deleteOutput := func(_ context.Context, id string) error {
		outputDeletes++
		if id != claim.ClaimID {
			t.Fatal("deleted current/ready attempt")
		}
		return nil
	}
	if err := store.ReconcileSources(ctx, deleteBlob, deleteOutput); err != nil {
		t.Fatal(err)
	}
	if blobDeletes != 0 || outputDeletes != 1 {
		t.Fatalf("retry source deleted: %d %d", blobDeletes, outputDeletes)
	}
	if err := store.Retry(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimSourceProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inv := testRecordingPackage(t, again.ClaimID, p.ActorID, p.RecordingID).Inventory
	if err := store.FinishSourceProcessing(ctx, p.ID, again.ClaimID, inv); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("provider unavailable")
	if err := store.ReconcileSources(ctx, func(context.Context, string, []string) error { return fail }, deleteOutput); !errors.Is(err, fail) {
		t.Fatalf("cleanup failure lost: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET cleanup_after=clock_timestamp()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileSources(ctx, deleteBlob, deleteOutput); err != nil {
		t.Fatal(err)
	}
	if blobDeletes != 1 {
		t.Fatal("ready source not cleaned")
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.State != "ready" || got.PackageID != again.ClaimID {
		t.Fatalf("cleanup changed ready: %+v %v", got, err)
	}
}
