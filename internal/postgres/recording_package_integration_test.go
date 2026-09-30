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
