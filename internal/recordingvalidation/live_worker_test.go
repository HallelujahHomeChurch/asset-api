package recordingvalidation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"hhc/asset-api/internal/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// A serial Job leaves the second global slot idle while another capture waits.
func TestLiveWorkerDrainsTwoCapturesWithinGlobalSlotsAndJoinsCancellation(t *testing.T) {
	db, store, objects, captures := liveWorkerFixture(t)
	probe := PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/false", FFprobe: "/usr/bin/false", ScratchRoot: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan error, 1)
	go func() { _, err := RunLiveValidation(ctx, store, objects, probe); done <- err; close(done) }()
	defer func() {
		cancel()
		for range done {
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-objects.started:
		case <-ctx.Done():
			t.Fatal("second capture did not start while the first capture was waiting on provider I/O")
		}
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM recording_processing_slots WHERE leased_until>clock_timestamp()`).Scan(&active); err != nil || active != 2 {
		t.Fatalf("global slots: %d %v", active, err)
	}
	select {
	case id := <-objects.started:
		t.Fatalf("third capture exceeded the two-slot budget: %s", id)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if objects.active.Load() != 0 || objects.published.Load() != 0 {
		t.Fatal("returned before provider work joined or published cancelled bytes")
	}
	entries, err := os.ReadDir(probe.ScratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch cleanup: %v %v", entries, err)
	}
	for _, id := range captures {
		progress, err := store.LiveProgress(context.Background(), id)
		if err != nil || progress.LastSequence != -1 {
			t.Fatalf("unverified waterline advanced: %+v %v", progress, err)
		}
	}
}

type liveWorkerObjects struct {
	*packageMemoryObjects
	started     chan string
	mu          sync.Mutex
	seen        map[string]bool
	active      atomic.Int64
	published   atomic.Int64
	passThrough bool
	failCapture string
	release     chan struct{}
}

func (s *liveWorkerObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.passThrough {
		return s.packageMemoryObjects.Open(ctx, key)
	}
	s.active.Add(1)
	defer s.active.Add(-1)
	id := strings.Split(key, "/")[2]
	s.mu.Lock()
	if !s.seen[id] {
		s.seen[id] = true
		s.started <- id
	}
	s.mu.Unlock()
	if id == s.failCapture {
		return nil, assets.ErrInvalidUpload
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return nil, errors.New("provider temporarily unavailable")
	}
}

func TestLiveWorkerKeepsSiblingCaptureRunningAfterInvalidMedia(t *testing.T) {
	db, store, objects, ids := liveWorkerFixture(t)
	objects.failCapture = ids[0]
	objects.release = make(chan struct{})
	probe := PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/false", FFprobe: "/usr/bin/false", ScratchRoot: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan error, 1)
	go func() { _, err := RunLiveValidation(ctx, store, objects, probe); done <- err; close(done) }()
	defer func() {
		cancel()
		for range done {
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-objects.started:
		case <-ctx.Done():
			t.Fatal("sibling capture never started")
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state string
		if err := db.QueryRow(`SELECT state FROM recording_captures WHERE id=$1`, ids[0]).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "failed" {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("invalid capture never failed closed")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("worker abandoned its running sibling: %v", err)
	default:
	}
	close(objects.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM recording_processing_slots WHERE leased_until>clock_timestamp()`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("completed claims leaked slots: %d %v", active, err)
	}
	if objects.published.Load() != 0 || objects.active.Load() != 0 {
		t.Fatal("invalid/transient batch published or provider work was not joined")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM recording_captures WHERE id=$1`, ids[1]).Scan(&state); err != nil || state != "uploading" {
		t.Fatalf("transient sibling terminalized: %s %v", state, err)
	}
}
func (s *liveWorkerObjects) PublishLiveObject(ctx context.Context, from, to, etag string) error {
	s.published.Add(1)
	return s.packageMemoryObjects.CopyPackageObject(ctx, from, to, etag)
}
func (s *liveWorkerObjects) PutLivePlaylist(context.Context, string, int64, string, []byte) error {
	s.published.Add(1)
	return nil
}
func (s *liveWorkerObjects) AdvanceLivePointer(context.Context, string, int64, int) error {
	s.published.Add(1)
	return nil
}

func TestLiveWorkerPublishesTwoFullyDecoded2997Captures(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	probe, pack, bytes, declarations, _ := liveMovingMediaFixture(t)
	_, store, objects, ids := liveWorkerMediaFixture(t, pack, bytes, declarations)
	objects.passThrough = true
	probe.Objects = objects
	started := time.Now()
	processed, err := RunLiveValidation(context.Background(), store, objects, probe)
	if err != nil || !processed {
		t.Fatalf("paired live decode: %t %v", processed, err)
	}
	for _, id := range ids[:2] {
		progress, err := store.LiveProgress(context.Background(), id)
		if err != nil || progress.LastSequence != 0 || progress.MediaEndSeconds < 30 || progress.MediaEndSeconds > 30.04 {
			t.Fatalf("published timeline: %+v %v", progress, err)
		}
		for _, declaration := range declarations {
			key := "recordings/captures/" + id + "/final/" + declaration.Path
			if size, _, err := objects.Head(context.Background(), key); err != nil || size != declaration.SizeBytes {
				t.Fatalf("stable bytes: %d %v", size, err)
			}
		}
	}
	pending, err := store.LiveProgress(context.Background(), ids[2])
	if err != nil || pending.LastSequence != -1 {
		t.Fatalf("third capture exceeded admission bound: %+v %v", pending, err)
	}
	t.Logf("two complete moving-media 29.97fps batches: %s", time.Since(started))
}

func liveWorkerFixture(t *testing.T) (*sql.DB, *postgres.RecordingCaptureStore, *liveWorkerObjects, []string) {
	pack, bytes, declarations := liveIntakeFixture()
	return liveWorkerMediaFixture(t, pack, bytes, declarations)
}

func liveWorkerMediaFixture(t testing.TB, pack assets.RecordingPackage, bytes *packageMemoryObjects, declarations []assets.RecordingPackageObject) (*sql.DB, *postgres.RecordingCaptureStore, *liveWorkerObjects, []string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("asset_live_worker_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(3)
	t.Cleanup(func() { db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	store := postgres.NewRecordingCaptureStore(db)
	objects := &liveWorkerObjects{packageMemoryObjects: &packageMemoryObjects{bytes: map[string][]byte{}}, started: make(chan string, 3), seen: map[string]bool{}}
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)}
	for index, id := range ids {
		now := time.Now().UTC()
		capture := assets.RecordingCapture{ID: id, ActorID: fmt.Sprintf("actor-%d", index), RecordingID: fmt.Sprintf("recording-%d", index), CreateKey: "create-" + id, State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
		if _, err := store.CreateCapture(context.Background(), capture); err != nil {
			t.Fatal(err)
		}
		_, err := store.UpdateCapture(context.Background(), id, func(c *assets.RecordingCapture) error {
			for _, declaration := range declarations {
				c.Objects = append(c.Objects, assets.RecordingCaptureObject{RecordingPackageObject: declaration, State: "queued"})
				c.DeclaredBytes += declaration.SizeBytes
				objects.bytes["recordings/packages/"+id+"/staging/"+declaration.Path] = bytes.bytes[pack.StagingKey(declaration.Path)]
			}
			c.DeclaredObjects = len(c.Objects)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return db, store, objects, ids
}
