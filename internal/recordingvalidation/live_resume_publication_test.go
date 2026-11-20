package recordingvalidation

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type ambiguousLivePointer struct {
	*liveWorkerObjects
	revision int64
	tail     int
	calls    int
}

func (o *ambiguousLivePointer) AdvanceLivePointer(_ context.Context, _ string, revision int64, tail int) error {
	o.calls++
	if o.calls == 1 {
		o.revision, o.tail = revision, tail
		return context.DeadlineExceeded // Provider stored the pointer but lost its response.
	}
	if revision != o.revision || tail != o.tail {
		return errors.New("replayed another pointer")
	}
	return nil
}

func TestResumeCommittedLiveBatchAfterPointerResponseIsLost(t *testing.T) {
	db, store, objects, ids := liveWorkerFixture(t)
	// This fixture intentionally starts at the durable boundary after stable media
	// was committed. Retrying an immutable revision must never reread/encode it.
	ctx := context.Background()
	if _, err := db.Exec(`UPDATE recording_captures SET state='failed',terminal_at=now() WHERE id<>$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	batch := map[string]assets.RecordingLiveFragment{}
	for _, r := range assets.LiveRenditions() {
		batch[r.Name] = assets.RecordingLiveFragment{Start: 0, End: 30, FrameRate: 30, Codecs: "avc1.64001f,mp4a.40.2"}
	}
	snapshot, err := store.CommitLiveBatch(ctx, claim, batch)
	if err != nil {
		t.Fatal(err)
	}
	claim.Snapshot = snapshot
	pointer := &ambiguousLivePointer{liveWorkerObjects: objects}
	if err = runLiveClaim(ctx, store, pointer, PackageMediaProbe{}, claim); err != nil {
		t.Fatal(err)
	}
	progress, err := store.LiveProgress(ctx, ids[0])
	if err != nil || progress.LastSequence != -1 {
		t.Fatalf("exposed unacknowledged progress: %+v %v", progress, err)
	}
	if _, err = db.Exec(`UPDATE recording_live SET next_attempt_at=now() WHERE capture_id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	work, err := ClaimLiveWork(ctx, store, pointer, PackageMediaProbe{})
	if err != nil || work == nil {
		t.Fatalf("durable retry missing: %v", err)
	}
	if err = work(); err != nil {
		t.Fatal(err)
	}
	progress, err = store.LiveProgress(ctx, ids[0])
	if err != nil || progress.LastSequence != 0 || pointer.tail != 0 || pointer.calls != 2 || progress.LastAdvancedAt == nil || progress.LastAdvancedAt.After(time.Now()) {
		t.Fatalf("retry lost durable tail: %+v %v", progress, err)
	}
	if err = store.FinishLivePublication(ctx, claim, snapshot.Revision); err == nil {
		t.Fatal("stale claimant committed twice")
	}
}
