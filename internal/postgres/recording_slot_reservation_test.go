package postgres

import (
	"context"
	"database/sql"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestKnownBroadcastKeepsCapacityAcrossArchiveAdmissionAndIdleExpiry(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	for _, id := range []string{"first", "second"} {
		p := testRecordingPackage(t, id, id, id)
		if err := packages.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	capture := assets.RecordingCapture{ID: strings.Repeat("d", 32), ActorID: "actor", RecordingID: "active", CreateKey: "slot-demand", State: "uploading", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := NewRecordingCaptureStore(db).WithBroadcastObjects(&broadcastMemoryObjects{values: map[string][]byte{}}).CreateCapture(ctx, capture); err != nil {
		t.Fatal(err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); err != nil {
		t.Fatal("first background job", err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("second background occupied protected slot: %v", err)
	}
	if _, err := db.Exec("UPDATE recording_captures SET created_at=now()-interval '30 minutes' WHERE id=$1", capture.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); err != nil {
		t.Fatal("idle broadcast permanently withheld capacity", err)
	}
}

func TestReservedSlotAllowsUntilActiveBackgroundDrainsAndReadyWorkStaysDiscoverable(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	c := assets.RecordingCapture{ID: strings.Repeat("f", 32), ActorID: "actor", RecordingID: "active", CreateKey: "late-claim", State: "uploading", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := NewRecordingCaptureStore(db).WithBroadcastObjects(&broadcastMemoryObjects{values: map[string][]byte{}}).CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a slot-2 claim which was admitted before the producer bound.
	exec("UPDATE recording_processing_slots SET job_id='earlier-background',claim_id='old',leased_until=now()+interval '2 minutes' WHERE slot=2")
	packageStore := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "waiting", "actor", "rec")
	if err := packageStore.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packageStore.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	events := NewRecordingEventStore(db, false)
	if ready, err := events.workReady(ctx, "processing"); err != nil || ready {
		t.Fatalf("protected slot repeatedly dispatched: %v %v", ready, err)
	}
	var mediaSlot int
	if err := db.QueryRow(claimLiveSlot).Scan(&mediaSlot); err != nil || mediaSlot != 1 {
		t.Fatalf("old background in slot 2 hid free slot: %d %v", mediaSlot, err)
	}
	exec("UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE slot=2")
	if ready, err := events.workReady(ctx, "processing"); err != nil || !ready {
		t.Fatalf("waiting package stranded: %v %v", ready, err)
	}
	exec("UPDATE recording_processing_slots SET job_id='live:another',claim_id='live',leased_until=now()+interval '2 minutes' WHERE slot=2")
	if err := db.QueryRow(claimLiveSlot).Scan(&mediaSlot); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("live displaced the queued package: %v", err)
	}
	if _, err := packageStore.ClaimPackageValidation(ctx); err != nil {
		t.Fatal("background work failed to progress", err)
	}
}

func TestProtectedWakeDoesNotDispatchTwoBackgroundLanes(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	c := assets.RecordingCapture{ID: strings.Repeat("e", 32), ActorID: "actor", RecordingID: "active", CreateKey: "wake-reservation", State: "uploading", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := NewRecordingCaptureStore(db).WithBroadcastObjects(&broadcastMemoryObjects{values: map[string][]byte{}}).CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	events := NewRecordingEventStore(db, false)
	mediaReady := false
	events.ready = func(ctx context.Context, lane string) (bool, error) { return lane != "live" || mediaReady, nil }
	e, ok, err := events.ClaimDispatch(ctx)
	if err != nil || !ok || e.Lane == "live" {
		t.Fatal(e, ok, err)
	}
	if e, ok, err = events.ClaimDispatch(ctx); err != nil || ok {
		t.Fatalf("two non-live wakes displaced producer: %v %v %v", e, ok, err)
	}
	mediaReady = true
	if err = events.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if e, ok, err = events.ClaimDispatch(ctx); err != nil || !ok || e.Lane != "live" {
		t.Fatalf("live wake stranded: %v %v %v", e, ok, err)
	}
}
