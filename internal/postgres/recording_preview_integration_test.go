package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestPreviewBackfillSharedSlotsFencingAndCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("preview-%d", i)
		p := testRecordingPackage(t, id, id, id)
		if err := store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := store.Freeze(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
		c, err := store.ClaimPackageValidation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishPackageValidation(ctx, c.Package.ID, c.ClaimID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Competing scheduled executions must never claim the same package or
	// create more than two global processing leases.
	type result struct {
		claim RecordingPackageClaim
		err   error
	}
	results := make(chan result, 3)
	for i := 0; i < 3; i++ {
		go func() { c, err := store.ClaimPackagePreview(ctx); results <- result{c, err} }()
	}
	var claimed []RecordingPackageClaim
	for i := 0; i < 3; i++ {
		r := <-results
		if r.err == nil {
			claimed = append(claimed, r.claim)
		} else if !errors.Is(r.err, assets.ErrNotFound) {
			t.Fatal(r.err)
		}
	}
	if len(claimed) != 2 || claimed[0].Package.ID == claimed[1].Package.ID {
		t.Fatalf("concurrent claims: %+v", claimed)
	}
	a, b := claimed[0], claimed[1]
	if _, err := store.ClaimPackagePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("third slot: %v", err)
	}
	if err := store.HeartbeatPackagePreview(ctx, a.Package.ID, a.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_processing_slots SET leased_until=clock_timestamp()-interval '1 second' WHERE claim_id=$1`, a.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET preview_claimed_until=clock_timestamp()-interval '1 second' WHERE id=$1`, a.Package.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimPackagePreview(ctx)
	if err != nil || reclaimed.Package.ID != a.Package.ID || reclaimed.ClaimID == a.ClaimID {
		t.Fatalf("reclaim: %+v %v", reclaimed, err)
	}
	published := 0
	publish := func(context.Context) error { published++; return nil }
	if err := store.FinishPackagePreview(ctx, a.Package.ID, a.ClaimID, true, publish); !errors.Is(err, assets.ErrConflict) || published != 0 {
		t.Fatalf("stale publish: %v", err)
	}
	if err := store.FinishPackagePreview(ctx, reclaimed.Package.ID, reclaimed.ClaimID, true, publish); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackagePreview(ctx, b.Package.ID, b.ClaimID, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.Package.ID, b.Package.ID} {
		p, err := store.Get(ctx, id)
		if err != nil || p.State != "ready" {
			t.Fatalf("video changed: %+v %v", p, err)
		}
	}
	upload := testRecordingPackage(t, "priority-upload", "priority-actor", "priority-recording")
	if err := store.Create(ctx, upload); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze(ctx, upload.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPackagePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("preview bypassed waiting upload: %v", err)
	}
	validation, err := store.ClaimPackageValidation(ctx)
	if err != nil || validation.Package.ID != upload.ID {
		t.Fatalf("preview occupied validation capacity: %v", err)
	}
	var deleted []string
	remove := func(_ context.Context, keys []string) error { deleted = append(deleted, keys...); return nil }
	if err := store.CleanupPackage(ctx, a.Package.ID, remove); err != nil {
		t.Fatal(err)
	}
	for _, key := range deleted {
		if strings.Contains(key, "previews/") {
			t.Fatal("live preview deleted")
		}
	}
	if _, err := db.Exec(`UPDATE recording_packages SET ready_at=now()-interval '31 days',media_expires_at=now()-interval '1 day',cleanup_after=now()-interval '1 second' WHERE id=$1`, a.Package.ID); err != nil {
		t.Fatal(err)
	}
	// Keep the exact ready metadata constraint when changing fixture timestamps.
	if err := store.CleanupPackage(ctx, a.Package.ID, remove); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, key := range deleted {
		if strings.HasSuffix(key, "previews/current.json") {
			found = true
		}
	}
	if !found {
		t.Fatal("expired previews retained")
	}
}

func TestPreviewMigrationBackfillsOldReadyAndRetriesAreBounded(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	// Apply the old schema first, create a ready package, then apply 031.
	paths, err := filepath.Glob("../migrations/sql/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.Contains(path, "031_") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "old-ready", "actor-old", "recording-old")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageValidation(ctx, p.ID, c.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../migrations/sql/031_recording_previews.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c, err := store.ClaimPackagePreview(ctx)
		if err != nil || c.Package.ID != p.ID {
			t.Fatalf("attempt %d: %+v %v", i, c, err)
		}
		if err := store.FinishPackagePreview(ctx, p.ID, c.ClaimID, false, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimPackagePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
			t.Fatalf("backoff: %v", err)
		}
		if _, err := db.Exec(`UPDATE recording_packages SET preview_next_attempt_at=now()-interval '1 minute' WHERE id=$1`, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ClaimPackagePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("fourth attempt: %v", err)
	}
	var state, preview string
	var attempts int
	if err := db.QueryRow(`SELECT state,preview_state,preview_attempts FROM recording_packages WHERE id=$1`, p.ID).Scan(&state, &preview, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || preview != "failed" || attempts != 3 {
		t.Fatalf("%s %s %d", state, preview, attempts)
	}
}
