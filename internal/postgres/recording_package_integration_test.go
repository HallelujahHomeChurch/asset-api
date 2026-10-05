package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func testRecordingPackage(t *testing.T, id, actor, recording string) assets.RecordingPackage {
	t.Helper()
	inv := assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1", Objects: []assets.RecordingPackageObject{
		{Path: "master.m3u8", SizeBytes: 100, SHA256: strings.Repeat("a", 64)},
		{Path: "720p/index.m3u8", SizeBytes: 100, SHA256: strings.Repeat("b", 64)},
		{Path: "720p/init.mp4", SizeBytes: 100, SHA256: strings.Repeat("c", 64)},
		{Path: "720p/seg-000000.m4s", SizeBytes: 100, SHA256: strings.Repeat("d", 64)},
	}, Renditions: []assets.RecordingRendition{{Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}}}
	inv.InventoryDigest, _ = assets.RecordingInventoryDigest(inv)
	size, err := assets.ValidateRecordingInventory(inv)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return assets.RecordingPackage{ID: id, SessionID: id, OwnerService: "hhc-web-api", ActorID: actor, RecordingID: recording, IdempotencyKey: "operation-" + id, State: "uploading", SizeBytes: size, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), Inventory: inv}
}

func TestRecordingPackagePersistenceAndDurableFreeze(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "package-a", "actor-a", "recording-a")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := store.FindByIdempotency(ctx, p.IdempotencyKey)
	if err != nil || got.ID != p.ID || got.Inventory.InventoryDigest != p.Inventory.InventoryDigest || len(got.Inventory.Objects) != 4 {
		t.Fatalf("persist: %+v %v", got, err)
	}
	// Reuse the existing lock's connection; a one-connection pool must not deadlock.
	db.SetMaxOpenConns(1)
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	interrupted := errors.New("response lost after durable freeze")
	err = store.WithSessionLock(lockCtx, p.ID, func(ctx context.Context) error {
		if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
			return err
		}
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	got, err = NewRecordingPackageStore(db).Get(ctx, p.ID)
	if err != nil || got.State != "freezing" {
		t.Fatalf("durable receipt: %+v %v", got, err)
	}
	if err := store.Freeze(ctx, p.ID, time.Now()); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("CAS: %v", err)
	}
}

func TestRecordingPackageCapsAndExpiryAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	var group sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		p := testRecordingPackage(t, fmt.Sprintf("package-%d", i), "same-actor", fmt.Sprintf("recording-%d", i))
		group.Add(1)
		go func() { defer group.Done(); results <- store.Create(ctx, p) }()
	}
	group.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, assets.ErrConflict) {
			t.Fatal(err)
		}
	}
	if created != 2 {
		t.Fatalf("created %d packages; expected actor cap 2", created)
	}
	duplicate := testRecordingPackage(t, "duplicate", "other-actor", "recording-unique")
	if err := store.Create(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	duplicate.ID = "duplicate-two"
	duplicate.SessionID = duplicate.ID
	duplicate.IdempotencyKey = "different-operation"
	if err := store.Create(ctx, duplicate); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("recording cap: %v", err)
	}
	if err := store.Freeze(ctx, "duplicate", duplicate.ExpiresAt); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("expired freeze: %v", err)
	}
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPackageJobsGlobalSlotsAndStaleClaimFencing(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	for i := 0; i < 3; i++ {
		p := testRecordingPackage(t, fmt.Sprintf("lease-package-%d", i), fmt.Sprintf("actor-%d", i), fmt.Sprintf("lease-recording-%d", i))
		if err := store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	a, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Package.ID == b.Package.ID || a.ClaimID == b.ClaimID {
		t.Fatal("duplicate claim")
	}
	if _, err := store.ClaimPackageValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("third concurrent job: %v", err)
	}
	if err := store.HeartbeatPackageValidation(ctx, a.Package.ID, a.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET claimed_until=now()-interval '1 minute' WHERE id=$1`, a.Package.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=now()-interval '1 minute' WHERE claim_id=$1`, a.ClaimID); err != nil {
		t.Fatal(err)
	}
	c, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Package.ID != a.Package.ID || c.ClaimID == a.ClaimID || c.Attempts != 2 {
		t.Fatalf("reclaim: %+v", c)
	}
	if err := store.HeartbeatPackageValidation(ctx, a.Package.ID, a.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if err := store.FinishPackageValidation(ctx, a.Package.ID, a.ClaimID, true, ""); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale ready: %v", err)
	}
	if err := store.FinishPackageValidation(ctx, c.Package.ID, c.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, c.Package.ID)
	if err != nil || got.State != "ready" {
		t.Fatalf("ready: %+v %v", got, err)
	}
	var prefix string
	var retention time.Duration
	if err := db.QueryRowContext(ctx, `SELECT final_prefix,(extract(epoch from(media_expires_at-ready_at))*1000000000)::bigint FROM recording_packages WHERE id=$1`, c.Package.ID).Scan(&prefix, &retention); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prefix, "/final/"+c.ClaimID+"/") || retention != 30*24*time.Hour {
		t.Fatalf("retention %s %s", prefix, retention)
	}
	if _, err := store.ClaimPackageValidation(ctx); err != nil {
		t.Fatalf("released slot: %v", err)
	}
}

func TestPackageJobsConcurrentClaimIsGloballyBounded(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	for i := 0; i < 8; i++ {
		p := testRecordingPackage(t, fmt.Sprintf("parallel-package-%d", i), fmt.Sprintf("actor-%d", i), fmt.Sprintf("parallel-recording-%d", i))
		if err := store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, err := store.ClaimPackageValidation(ctx); results <- err }()
	}
	group.Wait()
	close(results)
	claimed := 0
	for err := range results {
		if err == nil {
			claimed++
		} else if !errors.Is(err, assets.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if claimed != 2 {
		t.Fatalf("concurrent claims: %d", claimed)
	}
}

func TestReadyPackagePurgeRetainsFinalAndSweepsLateStagingWrites(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "cleanup-package", "actor-a", "cleanup-recording")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	bytes := map[string]bool{}
	prefix := "recordings/packages/" + p.ID + "/final/" + claim.ClaimID + "/"
	for _, o := range p.Inventory.Objects {
		bytes[p.StagingKey(o.Path)] = true
		bytes[prefix+o.Path] = true
	}
	bytes[prefix+"package.json"] = true
	deleteBatch := func(_ context.Context, keys []string) error {
		for _, key := range keys {
			delete(bytes, key)
		}
		return nil
	}
	if err := store.CleanupPackage(ctx, p.ID, deleteBatch); err != nil {
		t.Fatal(err)
	}
	if bytes[p.StagingKey("master.m3u8")] || !bytes[prefix+"master.m3u8"] {
		t.Fatal("ready final removed or staging retained")
	}
	bytes[p.StagingKey("master.m3u8")] = true
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=now()-interval '1 minute',expires_at=now()-interval '1 minute',created_at=now()-interval '24 hours' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupPackage(ctx, p.ID, deleteBatch); err != nil {
		t.Fatal(err)
	}
	if bytes[p.StagingKey("master.m3u8")] || !bytes[prefix+"master.m3u8"] {
		t.Fatal("late write not swept or ready final removed")
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=now()-interval '1 minute',ready_at=now()-interval '30 days 30 minutes',media_expires_at=now()-interval '30 minutes' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupPackage(ctx, p.ID, deleteBatch); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.State != "expired" || !bytes[prefix+"master.m3u8"] {
		t.Fatalf("expiry/grant grace: %+v %v", got, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=now()-interval '1 minute',ready_at=now()-interval '31 days',media_expires_at=now()-interval '1 day' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupPackage(ctx, p.ID, deleteBatch); err != nil {
		t.Fatal(err)
	}
	if len(bytes) != 0 {
		t.Fatalf("expired bytes retained: %v", bytes)
	}
}

func TestPackageCleanupRetriesAndNeverDeletesActiveAttempt(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "retry-cleanup", "actor-a", "retry-recording")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	active, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recording_package_attempts(claim_id,package_id,state,finished_at) VALUES ('abandoned-attempt',$1,'abandoned',now()-interval '7 hours')`, p.ID); err != nil {
		t.Fatal(err)
	}
	providerFailure := errors.New("transient delete failure")
	if err := store.CleanupPackage(ctx, p.ID, func(context.Context, []string) error { return providerFailure }); !errors.Is(err, providerFailure) {
		t.Fatalf("delete failure: %v", err)
	}
	ids, err := store.PackageCleanupCandidates(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("failed item immediately retried: %v %v", ids, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=clock_timestamp()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	ids, err = store.PackageCleanupCandidates(ctx)
	if err != nil || len(ids) != 1 || ids[0] != p.ID {
		t.Fatalf("retry omitted: %v %v", ids, err)
	}
	deleted := 0
	if err := store.CleanupPackage(ctx, p.ID, func(_ context.Context, keys []string) error {
		for _, key := range keys {
			if strings.Contains(key, "/staging/") || strings.Contains(key, active.ClaimID) || !strings.Contains(key, "/final/abandoned-attempt/") {
				t.Fatalf("active scope deleted: %s", key)
			}
			deleted++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if deleted != len(p.Inventory.Objects)+1 {
		t.Fatalf("deleted %d keys", deleted)
	}
	if err := store.HeartbeatPackageValidation(ctx, p.ID, active.ClaimID); err != nil {
		t.Fatalf("active job affected: %v", err)
	}
}
