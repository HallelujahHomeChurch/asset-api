package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"slices"
	"sync"
	"testing"
)

func TestCoverLifecycleIsOwnerBoundIdempotentAndFenced(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	pkg := testRecordingPackage(t, "cover-package", "actor", "cover-recording")
	if err := packages.Create(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET state='ready',completed_at=now(),media_expires_at=now()+interval '30 days',ready_at=now(),final_prefix='recordings/packages/cover-package/final/one/' WHERE id='cover-package'`); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCoverStore(db)
	page, err := store.Backfill(ctx, "", 1, false)
	if err != nil || len(page) != 1 || page[0] != pkg.ID {
		t.Fatalf("dry run: %v %v", page, err)
	}
	items, err := store.List(ctx, pkg.ID, pkg.RecordingID)
	if err != nil || len(items) != 0 {
		t.Fatalf("dry run wrote: %v %v", items, err)
	}
	first, err := store.Create(ctx, pkg.ID, pkg.RecordingID, "actor", "upload-one", "image/png", "digest")
	if err != nil {
		t.Fatal(err)
	}
	same, err := store.Create(ctx, pkg.ID, pkg.RecordingID, "actor", "upload-one", "image/png", "digest")
	if err != nil || same.ID != first.ID {
		t.Fatalf("replay %v %v", same, err)
	}
	if _, err := store.Create(ctx, pkg.ID, pkg.RecordingID, "actor", "upload-one", "image/png", "different"); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("changed bytes accepted", err)
	}
	if _, err := store.List(ctx, pkg.ID, "other-recording"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("owner mismatch", err)
	}
	if err := store.Queue(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ID != first.ID {
		t.Fatal("wrong claim")
	}
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, pkg.RecordingID); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, claim, true); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("late worker promoted deleted recording", err)
	}
	if _, err := store.Create(ctx, pkg.ID, pkg.RecordingID, "actor", "late-upload", "image/png", "digest"); err == nil {
		t.Fatal("deleted owner resurrected")
	}
	page, err = store.Backfill(ctx, "", 100, true)
	if err != nil || len(page) != 0 {
		t.Fatalf("backfill queued deleted owner: %v %v", page, err)
	}
}

func TestCoverReferenceProtectsCurrentAndCleanupRetries(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	pkg := testRecordingPackage(t, "cleanup-cover", "actor", "recording-cleanup")
	if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET state='ready',completed_at=now(),ready_at=now(),final_prefix='recordings/packages/cleanup-cover/final/one/',media_expires_at=now()+interval '30 days' WHERE id=$1`, pkg.ID); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingCoverStore(db)
	c, err := s.Create(ctx, pkg.ID, pkg.RecordingID, "actor", "key", "image/png", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Queue(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, claim, true); err != nil {
		t.Fatal(err)
	}
	page, err := s.Backfill(ctx, "", 100, true)
	if err != nil || len(page) != 1 {
		t.Fatalf("apply: %v %v", page, err)
	}
	page, err = s.Backfill(ctx, "", 100, true)
	if err != nil || len(page) != 0 {
		t.Fatalf("replay: %v %v", page, err)
	}
	if err := s.Retain(ctx, pkg.ID, pkg.RecordingID, c.ID, "selection-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, pkg.ID, pkg.RecordingID, c.ID, "abandoned-selection"); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, pkg.ID, pkg.RecordingID, c.ID, "abandoned-selection"); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("late retain resurrected released reference", err)
	}
	if _, err := db.Exec(`UPDATE recording_covers SET created_at=now()-interval '2 days' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := s.Reconcile(ctx, func(_ context.Context, k []string) error { keys = append(keys, k...); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != c.InputKey() {
		t.Fatalf("deleted live output: %v", keys)
	}
	var daily bool
	if err := db.QueryRow(`SELECT cleanup_after>now()+interval '23 hours' FROM recording_covers WHERE id=$1`, c.ID).Scan(&daily); err != nil || !daily {
		t.Fatalf("successful cleanup not daily: %v %v", daily, err)
	}
	if err := s.Reconcile(ctx, func(context.Context, []string) error { t.Fatal("successful cleanup immediately repeated"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, pkg.ID, pkg.RecordingID, c.ID, "selection-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, pkg.ID, pkg.RecordingID, c.ID); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("unretained stale cover remains readable", err)
	}
	items, err := s.List(ctx, pkg.ID, pkg.RecordingID)
	index := slices.IndexFunc(items, func(item RecordingCover) bool { return item.ID == c.ID })
	if err != nil || index < 0 || items[index].State != "expired" || items[index].OperationKey != c.OperationKey {
		t.Fatalf("missing bounded expired receipt before cleanup: %#v %v", items, err)
	}
	if err := s.Retain(ctx, pkg.ID, pkg.RecordingID, c.ID, "late-selection"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("expired unselected cover was retained", err)
	}
	if _, err := db.Exec(`UPDATE recording_covers SET cleanup_after=now() WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx, func(context.Context, []string) error { return errors.New("provider unavailable") }); err == nil {
		t.Fatal("provider error swallowed")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM recording_covers WHERE id=$1`, c.ID).Scan(&state); err != nil || state != "expired" {
		t.Fatalf("not fenced %s %v", state, err)
	}
	items, err = s.List(ctx, pkg.ID, pkg.RecordingID)
	index = slices.IndexFunc(items, func(item RecordingCover) bool { return item.ID == c.ID })
	if err != nil || index < 0 || items[index].State != "expired" {
		t.Fatalf("missing expired receipt after cleanup: %#v %v", items, err)
	}
}

func TestCoverConcurrentClaimsAndExhaustedLease(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	pkg := testRecordingPackage(t, "leases-cover", "actor", "leases-recording")
	if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET state='ready',completed_at=now(),ready_at=now(),final_prefix='recordings/packages/leases-cover/final/one/',media_expires_at=now()+interval '30 days' WHERE id=$1`, pkg.ID); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingCoverStore(db)
	c, err := s.Create(ctx, pkg.ID, pkg.RecordingID, "actor", "key", "image/png", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Queue(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	claims := make(chan RecordingCover, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := s.Claim(ctx)
			results <- err
			if err == nil {
				claims <- claim
			}
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, assets.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("claims=%d", success)
	}
	claim := <-claims
	if _, err := db.Exec(`UPDATE recording_covers SET attempts=3,claimed_until=now()-interval '1 second' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_processing_slots SET leased_until=now()-interval '1 second' WHERE job_id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, claim, true); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("expired worker promoted", err)
	}
	if _, err := s.Claim(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal(err)
	}
	state, err := s.Get(ctx, pkg.ID, pkg.RecordingID, c.ID)
	if err != nil || state.State != "failed" {
		t.Fatalf("exhausted job: %+v %v", state, err)
	}
}

func TestCoverCleanupPreservesNewDeletionAndContinuesAfterFailure(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	pkg := testRecordingPackage(t, "cleanup-race", "actor", "cleanup-race-recording")
	if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET state='ready',completed_at=now(),ready_at=now(),final_prefix='recordings/packages/cleanup-race/final/one/',media_expires_at=now()+interval '30 days' WHERE id=$1`, pkg.ID); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingCoverStore(db)
	for _, key := range []string{"a", "b"} {
		c, err := s.Create(ctx, pkg.ID, pkg.RecordingID, "actor", key, "image/png", "digest")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Queue(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		claim, err := s.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, claim, true); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	if err := s.Reconcile(ctx, func(context.Context, []string) error {
		calls++
		if calls == 1 {
			return errors.New("provider failure")
		}
		return NewRecordingDeletionStore(db).DeleteRecording(ctx, pkg.RecordingID)
	}); err == nil || calls != 2 {
		t.Fatalf("cleanup stopped after first failure: calls=%d err=%v", calls, err)
	}
	var due int
	if err := db.QueryRow(`SELECT count(*) FROM recording_covers WHERE recording_id=$1 AND cleanup_after<=now() AND state='expired'`, pkg.RecordingID).Scan(&due); err != nil || due != 2 {
		t.Fatalf("overwrote deletion schedule: %d %v", due, err)
	}
}
