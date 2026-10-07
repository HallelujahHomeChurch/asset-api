package postgres

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"hhc/asset-api/internal/storage/r2"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCaptureDurableQueueReceiptAndCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	now := time.Now().UTC()
	c := assets.RecordingCapture{ID: "capture-a", ActorID: "22222222-2222-4222-8222-222222222222", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "create-a", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	_, err := store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error {
		c.Objects = []assets.RecordingCaptureObject{{RecordingPackageObject: assets.RecordingPackageObject{Path: "720p/init.mp4", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}, State: "queued"}}
		c.DeclaredObjects = 1
		c.DeclaredBytes = 10
		c.Receipts["confirm-a"] = assets.RecordingCaptureStoredReceipt{Digest: "digest", Receipt: assets.RecordingCaptureReceipt{OperationKey: "confirm-a", Operation: "confirm", AcceptedAt: now, CaptureID: c.ID}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewRecordingCaptureStore(db).GetCapture(ctx, c.ID)
	if err != nil || len(got.Objects) != 1 || got.Objects[0].State != "queued" || got.Receipts["confirm-a"].Receipt.Operation != "confirm" {
		t.Fatalf("crash recovery: %+v %v", got, err)
	}
	_, err = store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error { c.DeclaredBytes = 42; return assets.ErrConflict })
	if !errors.Is(err, assets.ErrConflict) {
		t.Fatal(err)
	}
	got, _ = store.GetCapture(ctx, c.ID)
	if got.DeclaredBytes != 10 {
		t.Fatal("rollback lost")
	}
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, c.RecordingID); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetCapture(ctx, c.ID)
	if err != nil || got.State != "expired" {
		t.Fatalf("delete: %+v %v", got, err)
	}
	deleted := []string{}
	if err := store.ReconcileCaptures(ctx, func(_ context.Context, keys []string) error { deleted = append(deleted, keys...); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatal("failed media deleted before seven days")
	}
	if _, err = db.Exec(`UPDATE recording_captures SET created_at=now()-interval '10 days',expires_at=now()-interval '9 days',terminal_at=now()-interval '8 days' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileCaptures(ctx, func(_ context.Context, keys []string) error { deleted = append(deleted, keys...); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "recordings/packages/capture-a/staging/720p/init.mp4" {
		t.Fatalf("cleanup: %v", deleted)
	}
}

type captureObjectStore struct{ sizes map[string]int64 }

func (o captureObjectStore) PresignPackageObject(_ context.Context, _ string, _ int64, _ string, _ time.Duration) (r2.PresignedPart, error) {
	return r2.PresignedPart{URL: "https://synthetic.invalid", Method: "PUT"}, nil
}
func (o captureObjectStore) ListPackageObjects(_ context.Context, _ string, _ int) (map[string]int64, error) {
	return o.sizes, nil
}
func TestCaptureSealAtomicPackageAndAbortFence(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	objects := captureObjectStore{sizes: map[string]int64{}}
	fixedNow := time.Now().UTC().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	normalizedNow := fixedNow.Truncate(time.Microsecond)
	s := assets.NewRecordingCaptureService(store, objects, func() time.Time { return fixedNow })
	r, err := s.Create(ctx, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "create-a")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Capture.CreatedAt.Equal(normalizedNow) || !r.Capture.ExpiresAt.Equal(normalizedNow.Add(assets.RecordingUploadTTL)) || !r.Receipt.AcceptedAt.Equal(normalizedNow) {
		t.Fatalf("create timestamp not normalized at source: created=%s expiry=%s receipt=%s", r.Capture.CreatedAt, r.Capture.ExpiresAt, r.Receipt.AcceptedAt)
	}
	replayCreate, err := s.Create(ctx, r.Capture.RecordingID, "22222222-2222-4222-8222-222222222222", "create-a")
	if err != nil || !replayCreate.Capture.CreatedAt.Equal(r.Capture.CreatedAt) || !replayCreate.Capture.ExpiresAt.Equal(r.Capture.ExpiresAt) || !replayCreate.Receipt.AcceptedAt.Equal(r.Receipt.AcceptedAt) {
		t.Fatalf("create/replay timestamp mismatch: %v", err)
	}
	fixedNow = fixedNow.Add(5 * time.Minute)
	id := r.Capture.ID
	inv := testRecordingPackage(t, "unused", "22222222-2222-4222-8222-222222222222", "11111111-1111-4111-8111-111111111111").Inventory
	inv.Renditions = []assets.RecordingRendition{{Name: "1080p", Width: 1920, Height: 1080, FrameRate: 30, VideoBitrate: 3000000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}, {Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}, {Name: "480p", Width: 854, Height: 480, FrameRate: 30, VideoBitrate: 800000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}}
	inv.Objects = inv.Objects[:1]
	for _, rendition := range inv.Renditions {
		for _, path := range []string{"index.m3u8", "init.mp4", "seg-000000.m4s"} {
			inv.Objects = append(inv.Objects, assets.RecordingPackageObject{Path: rendition.Name + "/" + path, SizeBytes: 10, SHA256: strings.Repeat("a", 64)})
		}
	}
	inv.InventoryDigest, _ = assets.RecordingInventoryDigest(inv)
	if _, err = s.Declare(ctx, id, "22222222-2222-4222-8222-222222222222", "declare-a", inv.Objects); err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, o := range inv.Objects {
		paths = append(paths, o.Path)
		objects.sizes[o.Path] = o.SizeBytes
	}
	if _, err = s.Confirm(ctx, id, "22222222-2222-4222-8222-222222222222", "confirm-a", paths); err != nil {
		t.Fatal(err)
	}
	first, err := s.Seal(ctx, id, "22222222-2222-4222-8222-222222222222", "seal-a", true, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Receipt.AcceptedAt.Equal(fixedNow.Truncate(time.Microsecond)) {
		t.Fatal("seal receipt not normalized")
	}
	second, err := s.Seal(ctx, id, "22222222-2222-4222-8222-222222222222", "seal-a", true, inv)
	if err != nil || !second.Receipt.AcceptedAt.Equal(first.Receipt.AcceptedAt) {
		t.Fatalf("receipt replay: %+v %v", second, err)
	}
	p, err := NewRecordingPackageStore(db).Get(ctx, id)
	if err != nil || p.State != "freezing" || !p.ExpiresAt.Equal(r.Capture.ExpiresAt) || p.Inventory.InventoryDigest != inv.InventoryDigest {
		t.Fatalf("package insertion: %+v %v", p, err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM recording_packages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate package: %d %v", count, err)
	}
	objects.sizes[paths[0]]++
	if _, err = s.Sign(ctx, id, "22222222-2222-4222-8222-222222222222", paths[:1]); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("late signing: %v", err)
	}
	claim, err := NewRecordingPackageStore(db).ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Abort(ctx, id, "22222222-2222-4222-8222-222222222222", "abort-a", "user_abort"); err != nil {
		t.Fatal(err)
	}
	if err = NewRecordingPackageStore(db).FinishPackageValidation(ctx, id, claim.ClaimID, true, ""); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale ready commit: %v", err)
	}
	got, err := s.Get(ctx, id, "22222222-2222-4222-8222-222222222222", "", 1000)
	if err != nil || got.State != "aborted" {
		t.Fatalf("abort persists: %+v %v", got, err)
	}
}
func TestCaptureQuotaTransactionsRollback(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := assets.NewRecordingCaptureService(NewRecordingCaptureStore(db), captureObjectStore{}, time.Now)
	results := make(chan error, 6)
	var group sync.WaitGroup
	for i := 0; i < 6; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, err := s.Create(ctx, fmt.Sprintf("11111111-1111-4111-8111-%012d", i), "22222222-2222-4222-8222-222222222222", fmt.Sprintf("create-%d", i))
			results <- err
		}(i)
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
		t.Fatalf("actor cap: %d", created)
	}
	r, err := s.Create(ctx, "44444444-4444-4444-8444-444444444444", "33333333-3333-4333-8333-333333333333", "bulk")
	if err != nil {
		t.Fatal(err)
	}
	objects := []assets.RecordingPackageObject{}
	for i := 0; i < 75; i++ {
		objects = append(objects, assets.RecordingPackageObject{Path: fmt.Sprintf("720p/seg-%06d.m4s", i), SizeBytes: assets.RecordingObjectMaxBytes, SHA256: strings.Repeat("a", 64)})
	}
	if _, err = s.Declare(ctx, r.Capture.ID, "33333333-3333-4333-8333-333333333333", "bulk-declare", objects); !errors.Is(err, assets.ErrRecordingPackageTooLarge) {
		t.Fatalf("10GB: %v", err)
	}
	got, err := s.Get(ctx, r.Capture.ID, "33333333-3333-4333-8333-333333333333", "", 1000)
	if err != nil || got.DeclaredBytes != 0 || got.DeclaredObjects != 0 || len(got.Objects) != 0 {
		t.Fatalf("quota rollback: %+v %v", got, err)
	}
}

func TestCaptureExpiryFencesPackageReadyCommit(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "package-a", "actor-a", "recording-a")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id) VALUES('package-a','actor-a','recording-a','create-a','validating',clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour','package-a')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recording_live(capture_id) SELECT id FROM recording_captures ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if err = packages.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("late ready commit: %v", err)
	}
}

func TestCaptureFullPackageValidationProjectsVerifiedObjects(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "package-a", "actor-a", "recording-a")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id,declared_objects,declared_bytes) VALUES('package-a','actor-a','recording-a','create-a','freezing',clock_timestamp(),clock_timestamp()+interval '1 hour','package-a',4,400)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recording_live(capture_id) SELECT id FROM recording_captures ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for _, o := range p.Inventory.Objects {
		if _, err = db.Exec(`INSERT INTO recording_capture_objects(capture_id,path,size_bytes,sha256,state) VALUES($1,$2,$3,$4,'queued')`, p.ID, o.Path, o.SizeBytes, o.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = packages.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	c, err := NewRecordingCaptureStore(db).GetCapture(ctx, p.ID)
	if err != nil || c.State != "ready" {
		t.Fatal(err)
	}
	for _, o := range c.Objects {
		if o.State != "verified" {
			t.Fatalf("normal ready projection still %s", o.State)
		}
	}
}
