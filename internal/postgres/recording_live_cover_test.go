package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestLiveCoverCustomBeforePackageAndSharedReferences(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingLiveCoverStore(db)
	c, err := s.Create(ctx, "defaults", "", "operator", "upload", "image/png", "digest", "custom")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Create(ctx, "defaults", "", "operator", "upload", "image/png", "digest", "custom")
	if err != nil || replay.ID != c.ID {
		t.Fatal("replay changed upload", err)
	}
	if _, err = s.Create(ctx, "defaults", "", "operator", "upload", "image/png", "other", "custom"); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("accepted changed bytes", err)
	}
	if err = s.Queue(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, claim, "output-digest", true); err != nil {
		t.Fatal(err)
	}
	for i, char := range []string{"a", "b"} {
		cap := assets.RecordingCapture{ID: strings.Repeat(char, 32), ActorID: char, RecordingID: char + "-recording", CreateKey: char, State: "uploading", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
		if _, err := NewRecordingCaptureStore(db).CreateCapture(ctx, cap); err != nil {
			t.Fatal(err)
		}
		if err := s.Retain(ctx, c.ID, cap.ID, cap.RecordingID, "capture-"+char); err != nil {
			t.Fatal(i, err)
		}
	}
	if err = s.Retain(ctx, c.ID, "defaults", "", "default-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Release(ctx, c.ID, "defaults", "default-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Retain(ctx, c.ID, "defaults", "", "default-1"); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("resurrected released reference", err)
	}
	if _, err = s.Get(ctx, c.ID, strings.Repeat("b", 32), "b-recording"); err != nil {
		t.Fatal("lost shared image", err)
	}
	if _, err = s.Get(ctx, c.ID, strings.Repeat("a", 32), "b-recording"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("owner leak", err)
	}
	if _, err := db.Exec(`UPDATE recording_live_covers SET created_at=now()-interval '2 days' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	removed := 0
	if err = s.Reconcile(ctx, func(context.Context, []string) error { removed++; return nil }); err != nil || removed != 0 {
		t.Fatal("cleaned shared cover", err)
	}
	for _, char := range []string{"a", "b"} {
		if err = s.Release(ctx, c.ID, strings.Repeat(char, 32), "capture-"+char); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Reconcile(ctx, func(context.Context, []string) error { removed++; return nil }); err != nil || removed != 1 {
		t.Fatal("unreferenced cover not cleaned", removed, err)
	}
}

func TestLiveCoverLateWorkerCannotPublishAfterDeletion(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	cap := assets.RecordingCapture{ID: strings.Repeat("c", 32), ActorID: "actor", RecordingID: "recording", CreateKey: "create", State: "uploading", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := NewRecordingCaptureStore(db).CreateCapture(ctx, cap); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingLiveCoverStore(db)
	c, err := s.Create(ctx, cap.ID, cap.RecordingID, "actor", "custom", "image/png", "digest", "custom")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Queue(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, cap.RecordingID); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, claim, "output", true); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("deleted cover promoted", err)
	}
}

func TestLiveCoverPromotionIsPackageBoundAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 32)
	recording := "promote-recording"
	c := assets.RecordingCapture{ID: id, ActorID: "promote-actor", RecordingID: recording, CreateKey: "promote", State: "uploading", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	pkg := testRecordingPackage(t, id, c.ActorID, recording)
	if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET state='ready',completed_at=now(),ready_at=now(),final_prefix='recordings/packages/promote/final/one/',media_expires_at=now()+interval '30 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id) VALUES($1,$2,$3,'promote','ready',now(),now()+interval '1 hour',$1)`, id, c.ActorID, recording); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingLiveCoverStore(db)
	upload, err := store.Create(ctx, "defaults", "", "actor", "u", "image/png", "input", "custom")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Queue(ctx, upload.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, claim, "normalized", true); err != nil {
		t.Fatal(err)
	}
	if err = store.Retain(ctx, upload.ID, id, recording, "inherit"); err != nil {
		t.Fatal(err)
	}
	cover, err := store.Promote(ctx, upload.ID, id, recording)
	if err != nil {
		t.Fatal(err)
	}
	same, err := store.Promote(ctx, upload.ID, id, recording)
	if err != nil || same.ID != cover.ID {
		t.Fatal("duplicate promotion", err)
	}
	if _, err = store.Promote(ctx, upload.ID, strings.Repeat("e", 32), recording); err == nil {
		t.Fatal("cross-package promotion")
	}
	if cover.Kind != "custom" || cover.InheritedCoverID != upload.ID {
		t.Fatal("lost source", cover)
	}
}
