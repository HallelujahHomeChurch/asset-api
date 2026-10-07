package postgres

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestLiveProgressFencesUnpublishedAndStaleClaims(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	now := time.Now().UTC()
	c := assets.RecordingCapture{ID: strings.Repeat("a", 32), ActorID: "22222222-2222-4222-8222-222222222222", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "live-create", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimLiveValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("claimed missing renditions: %v", err)
	}
	_, err := store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error {
		for _, r := range assets.LiveRenditions() {
			for _, name := range []string{"init.mp4", "seg-000000.m4s", "seg-000001.m4s"} {
				c.Objects = append(c.Objects, assets.RecordingCaptureObject{RecordingPackageObject: assets.RecordingPackageObject{Path: r.Name + "/" + name, SizeBytes: 10, SHA256: strings.Repeat("b", 64)}, State: "queued"})
			}
		}
		c.DeclaredObjects = len(c.Objects)
		c.DeclaredBytes = int64(len(c.Objects) * 10)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimLiveValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("double claim: %v", err)
	}
	batch := map[string]assets.RecordingLiveFragment{}
	for _, r := range assets.LiveRenditions() {
		batch[r.Name] = assets.RecordingLiveFragment{Start: 0, End: 30, Codecs: "avc1.64001f,mp4a.40.2"}
	}
	snapshot, err := store.CommitLiveBatch(ctx, claim, batch)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := store.LiveProgress(ctx, c.ID)
	if err != nil || progress.LastSequence != -1 {
		t.Fatalf("unpublished watermark: %+v %v", progress, err)
	}
	if err := store.FinishLivePublication(ctx, claim, snapshot.Revision); err != nil {
		t.Fatal(err)
	}
	progress, err = store.LiveProgress(ctx, c.ID)
	if err != nil || progress.LastSequence != 0 || progress.MediaEndSeconds != 30 {
		t.Fatalf("published watermark: %+v %v", progress, err)
	}
	next, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishLivePublication(ctx, claim, snapshot.Revision); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale lease: %v", err)
	}
	_, err = store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error { c.State = "aborted"; c.TerminalAt = &now; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range batch {
		v.Start = 30
		v.End = 60
		batch[name] = v
	}
	if _, err := store.CommitLiveBatch(ctx, next, batch); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("aborted capture advanced: %v", err)
	}
	if got := fmt.Sprint(next.Sequence); got != "1" {
		t.Fatal(got)
	}
}

func TestLiveCleanupRetainsFailureGrantsAndLeases(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	now := time.Now().UTC()
	c := assets.RecordingCapture{ID: strings.Repeat("c", 32), ActorID: "actor", RecordingID: "recording", CreateKey: "cleanup-live", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error {
		c.DeclaredBytes = 10
		c.DeclaredObjects = 1
		c.Objects = []assets.RecordingCaptureObject{{RecordingPackageObject: assets.RecordingPackageObject{Path: "720p/init.mp4", SizeBytes: 10, SHA256: strings.Repeat("b", 64)}, State: "queued"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	exec := func(query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE recording_captures SET state='failed',created_at=now()-interval '10 days',expires_at=now()-interval '9 days',terminal_at=now()-interval '6 days' WHERE id=$1`)
	deleted := []string{}
	remove := func(_ context.Context, keys []string) error { deleted = append(deleted, keys...); return nil }
	checkHeld := func() {
		t.Helper()
		if err := store.ReconcileLive(ctx, remove); err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 0 {
			t.Fatalf("protected media removed: %v", deleted)
		}
	}
	checkHeld()
	exec(`UPDATE recording_captures SET terminal_at=now()-interval '8 days' WHERE id=$1`)
	exec(`UPDATE recording_live SET read_grant_until=now()+interval '1 minute' WHERE capture_id=$1`)
	checkHeld()
	exec(`UPDATE recording_live SET read_grant_until=NULL,claim_id='active',claimed_until=now()+interval '1 minute' WHERE capture_id=$1`)
	checkHeld()
	exec(`UPDATE recording_live SET claim_id=NULL,claimed_until=NULL WHERE capture_id=$1`)
	if err := store.ReconcileLive(ctx, remove); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 || !strings.HasSuffix(deleted[2], "current.json") {
		t.Fatalf("cleanup inventory: %v", deleted)
	}
}

func TestLiveNormalSealAfterPublishedFullSegmentAppendsEndOnly(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	now := time.Now().UTC()
	objects := captureObjectStore{sizes: map[string]int64{}}
	svc := assets.NewRecordingCaptureService(store, objects, func() time.Time { return now })
	result, err := svc.Create(ctx, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "end-only")
	if err != nil {
		t.Fatal(err)
	}
	id, actor := result.Capture.ID, "22222222-2222-4222-8222-222222222222"
	inv := assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1"}
	names := []string{"master.m3u8"}
	batch := map[string]assets.RecordingLiveFragment{}
	for _, r := range assets.LiveRenditions() {
		r.DurationSeconds = 30
		r.SegmentCount = 1
		inv.Renditions = append(inv.Renditions, r)
		for _, n := range []string{"index.m3u8", "init.mp4", "seg-000000.m4s"} {
			names = append(names, r.Name+"/"+n)
		}
		batch[r.Name] = assets.RecordingLiveFragment{Start: 0, End: 30, Codecs: "avc1.64001f,mp4a.40.2"}
	}
	for _, name := range names {
		inv.Objects = append(inv.Objects, assets.RecordingPackageObject{Path: name, SizeBytes: 10, SHA256: strings.Repeat("a", 64)})
		objects.sizes[name] = 10
	}
	inv.InventoryDigest, _ = assets.RecordingInventoryDigest(inv)
	if _, err = svc.Declare(ctx, id, actor, "declare", inv.Objects); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Confirm(ctx, id, actor, "confirm", names); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CommitLiveBatch(ctx, claim, batch)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishLivePublication(ctx, claim, snapshot.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Seal(ctx, id, actor, "seal", true, inv); err != nil {
		t.Fatal(err)
	}
	end, err := store.ClaimLiveValidation(ctx)
	if err != nil || !end.EndOnly {
		t.Fatalf("end only claim: %+v %v", end, err)
	}
	final, err := store.CommitLiveEnd(ctx, end)
	if err != nil || !final.Ended || final.Revision != 2 || len(final.Segments) != 1 {
		t.Fatalf("end only: %+v %v", final, err)
	}
	if err = store.FinishLivePublication(ctx, end, final.Revision); err != nil {
		t.Fatal(err)
	}
	progress, err := store.LiveProgress(ctx, id)
	if err != nil || !progress.Ended || progress.LastSequence != 0 || progress.EndedAt == nil {
		t.Fatalf("normal end: %+v %v", progress, err)
	}
	if _, err = store.ClaimLiveValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("ended capture claimed: %v", err)
	}
}
