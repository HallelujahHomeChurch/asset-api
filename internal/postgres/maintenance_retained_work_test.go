package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"hhc/asset-api/internal/recordingqueue"
)

// The current release has identical migration sources. Replay must not turn
// retained work into a new operation, reset its budget, or lose its next claim.
func TestMaintenanceRetainedDerivativeReplaysAndSettlesOnce(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	insertAsset(t, db, "retained-image", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET detected_mime_type='image/jpeg' WHERE id='retained-image'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_derivative_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES('retained-event','retained-image','etag-retained-image',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.ClaimDerivativeRequest(ctx, now, time.Minute)
	if err != nil || !ok {
		t.Fatal(first, ok, err)
	}
	due := now.Add(time.Minute)
	if err := store.ScheduleDerivativeRequestRetry(ctx, first.EventID, first.Attempts, "synthetic retry", due, now); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	resumed := New(db)
	if _, ok, err := resumed.ClaimDerivativeRequest(ctx, now, time.Minute); err != nil || ok {
		t.Fatal("migration bypassed dispatch delay", ok, err)
	}
	next, ok, err := resumed.ClaimDerivativeRequest(ctx, due, time.Minute)
	if err != nil || !ok || next.EventID != first.EventID || next.AssetID != first.AssetID || next.ETag != first.ETag || next.Attempts != first.Attempts+1 {
		t.Fatalf("retained dispatch changed: %+v %v %v", next, ok, err)
	}
	if err := resumed.MarkDerivativeRequestDelivered(ctx, first.EventID, first.Attempts, due); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale dispatch settled: %v", err)
	}
	if err := resumed.MarkDerivativeRequestDelivered(ctx, next.EventID, next.Attempts, due); err != nil {
		t.Fatal(err)
	}
	claimed, state, err := resumed.ClaimProcessing(ctx, next.AssetID, next.ETag, due, time.Minute)
	if err != nil || state != assets.ProcessingClaimed || claimed.ProcessingAttempts != 1 {
		t.Fatal("retained media claim", state, err)
	}
	if _, state, err := resumed.ClaimProcessing(ctx, next.AssetID, next.ETag, due, time.Minute); err != nil || state != assets.ProcessingDeferred {
		t.Fatal("duplicate media claim admitted", state, err)
	}
	values := []assets.Derivative{{AssetID: next.AssetID, Variant: "small", ObjectKey: "synthetic/final/small.jpg", MIMEType: "image/jpeg", Width: 1, Height: 1, SizeBytes: 1, ETag: "synthetic-output", CreatedAt: due}}
	if err := resumed.CompleteProcessing(ctx, next.AssetID, "wrong-etag", claimed.ProcessingAttempts, values, due); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("wrong version committed: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := resumed.CompleteProcessing(ctx, next.AssetID, next.ETag, claimed.ProcessingAttempts, values, due); err != nil {
			t.Fatal("idempotent settlement", err)
		}
	}
	var outputs int
	if err := db.QueryRow(`SELECT count(*) FROM asset_derivatives WHERE asset_id=$1`, next.AssetID).Scan(&outputs); err != nil || outputs != 1 {
		t.Fatal(outputs, err)
	}
	if _, ok, err := resumed.ClaimDerivativeRequest(ctx, due.Add(time.Minute), time.Minute); err != nil || ok {
		t.Fatal("delivered dispatch reclaimed", ok, err)
	}
	if _, state, err := resumed.ClaimProcessing(ctx, next.AssetID, next.ETag, due.Add(time.Minute), time.Minute); err != nil || state != assets.ProcessingTerminal {
		t.Fatal("completed media reclaimed", state, err)
	}
}

func TestMaintenanceRetainedRecordingWakePreservesPayloadAndExpiry(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "retained-package", "actor", "recording")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := packages.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wakes := NewRecordingEventStore(db, false)
	event, ok, err := wakes.ClaimDispatch(ctx)
	if err != nil || !ok || event.Lane != "processing" {
		t.Fatal(event, ok, err)
	}
	if err := wakes.Sent(ctx, event); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	// Queued wake is retained without starting media work; the real migrator replays.
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages = NewRecordingPackageStore(db)
	wakes = NewRecordingEventStore(db, false)
	after, err := packages.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != after.ID || before.SessionID != after.SessionID || before.IdempotencyKey != after.IdempotencyKey || !before.ExpiresAt.Equal(after.ExpiresAt) || !reflect.DeepEqual(before.Inventory, after.Inventory) {
		t.Fatal("migration changed retained authority")
	}
	decoded, err := recordingqueue.Decode(string(payload))
	if err != nil || decoded != event {
		t.Fatal("wake payload no longer accepted", err)
	}
	if ok, err := wakes.Begin(ctx, decoded); err != nil || !ok {
		t.Fatal("queued wake lost", ok, err)
	}
	if ok, err := wakes.Begin(ctx, decoded); err != nil || ok {
		t.Fatal("duplicate wake admitted", ok, err)
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil || claim.Attempts != 1 || claim.Package.ID != p.ID {
		t.Fatal("retained package claim", claim.Attempts, err)
	}
	if !reflect.DeepEqual(claim.Package.Inventory, before.Inventory) {
		t.Fatal("worker received altered media inventory")
	}
	if err := packages.FinishPackageValidation(ctx, p.ID, "stale-claim", true, ""); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("stale publication accepted", err)
	}
	// DB authority settlement only: no assertion that synthetic bytes were encoded/uploaded.
	if err := packages.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := wakes.Finish(ctx, decoded, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := wakes.Begin(ctx, decoded); err != nil || ok {
		t.Fatal("settled wake replayed", ok, err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("ready package claimed again", err)
	}
	ready, err := packages.Get(ctx, p.ID)
	if err != nil || ready.State != "ready" || !ready.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("settlement changed identity/expiry", err)
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM recording_processing_slots WHERE claim_id IS NOT NULL`).Scan(&active); err != nil || active != 0 {
		t.Fatal("media slot leaked", active, err)
	}
}
