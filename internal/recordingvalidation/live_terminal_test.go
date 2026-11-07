package recordingvalidation

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

func TestLiveWorkerJoinsAbortedCaptureAndContinues(t *testing.T) {
	db, store, objects, ids := liveWorkerFixture(t)
	objects.release = make(chan struct{})
	probe := PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/false", FFprobe: "/usr/bin/false", ScratchRoot: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { _, err := RunLiveValidation(ctx, store, objects, probe); done <- err; close(done) }()
	defer func() {
		cancel()
		for range done {
		}
	}()
	for range 2 {
		select {
		case <-objects.started:
		case <-ctx.Done():
			t.Fatal("claims did not start")
		}
	}
	now, reason := time.Now().UTC(), "capture_incomplete"
	if _, err := store.UpdateCapture(ctx, ids[0], func(c *assets.RecordingCapture) error {
		c.State, c.TerminalAt, c.TerminalReason = "aborted", &now, &reason
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(objects.release)
	if err := <-done; err != nil {
		t.Fatalf("terminal capture failed the worker: %v", err)
	}
	if objects.active.Load() != 0 || objects.published.Load() != 0 {
		t.Fatal("provider work was not joined or aborted bytes were published")
	}
	var state, terminalReason string
	if err := db.QueryRow(`SELECT state,terminal_reason FROM recording_captures WHERE id=$1`, ids[0]).Scan(&state, &terminalReason); err != nil || state != "aborted" || terminalReason != reason {
		t.Fatalf("terminal authority changed: %s %s %v", state, terminalReason, err)
	}
	var held int
	if err := db.QueryRow(`SELECT count(*) FROM recording_processing_slots WHERE claim_id IS NOT NULL`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("completed claims retained capacity: %d %v", held, err)
	}
	if progress, err := store.LiveProgress(ctx, ids[0]); err != nil || progress.LastSequence != -1 {
		t.Fatalf("aborted progress advanced: %+v %v", progress, err)
	}
	if processed, err := RunLiveValidation(ctx, store, objects, probe); !processed || err != nil {
		t.Fatalf("worker did not continue with waiting capture: %v %v", processed, err)
	}
}

type abortingLiveObjects struct {
	*liveWorkerObjects
	store *postgres.RecordingCaptureStore
	claim postgres.RecordingLiveClaim
	once  sync.Once
	err   error
}

func (s *abortingLiveObjects) PublishLiveObject(ctx context.Context, from, to, etag string) (string, error) {
	version, err := s.liveWorkerObjects.PublishLiveObject(ctx, from, to, etag)
	if err != nil {
		return "", err
	}
	s.once.Do(func() {
		now, reason := time.Now().UTC(), "capture_incomplete"
		_, s.err = s.store.UpdateCapture(ctx, s.claim.Capture.ID, func(c *assets.RecordingCapture) error {
			c.State, c.TerminalAt, c.TerminalReason = "aborted", &now, &reason
			return nil
		})
	})
	return version, s.err
}

func TestLiveWorkerAbortDuringStablePublicationDoesNotAdvancePointer(t *testing.T) {
	// Validate real media, then commit an abort during stable object copies.
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	probe, pack, media, declarations, _ := liveMovingMediaFixture(t, 300)
	db, store, objects, _ := liveWorkerMediaFixture(t, pack, media, declarations)
	if _, err := db.Exec(`UPDATE recording_captures SET segment_frames=300`); err != nil {
		t.Fatal(err)
	}
	objects.passThrough = true
	probe.Objects = objects
	ctx := context.Background()
	claim, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aborting := &abortingLiveObjects{liveWorkerObjects: objects, store: store, claim: claim}
	if err := runLiveClaim(ctx, store, aborting, probe, claim); err != nil {
		t.Fatalf("abort during publication failed worker: %v", err)
	}
	// Only immutable object copies ran, not playlists or the common pointer.
	if got := objects.published.Load(); got != int64(len(declarations)) {
		t.Fatalf("publication crossed the terminal fence: %d writes", got)
	}
	progress, err := store.LiveProgress(ctx, claim.Capture.ID)
	if err != nil || progress.LastSequence != -1 || progress.Revision != 0 {
		t.Fatalf("terminal capture advanced: %+v %v", progress, err)
	}
}

func TestLiveWorkerPreservesActiveClaimConflict(t *testing.T) {
	db, store, objects, _ := liveWorkerFixture(t)
	ctx := context.Background()
	claim, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_processing_slots SET leased_until=now()-interval '1 second' WHERE claim_id=$1`, claim.ClaimID); err != nil {
		t.Fatal(err)
	}
	objects.release = make(chan struct{})
	close(objects.release)
	probe := PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/false", FFprobe: "/usr/bin/false", ScratchRoot: t.TempDir()}
	if err := runLiveClaim(ctx, store, objects, probe, claim); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("active capture conflict hidden: %v", err)
	}
}

func TestTerminalLiveClaimCleanupPreservesAuthorityAndSuccessor(t *testing.T) {
	for _, state := range []string{"uploading", "aborted", "failed", "expired", "deadline", "deleted"} {
		t.Run(state, func(t *testing.T) {
			db, store, _, _ := liveWorkerFixture(t)
			ctx := context.Background()
			claim, err := store.ClaimLiveValidation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if state == "deleted" {
				err = postgres.NewRecordingDeletionStore(db).DeleteRecording(ctx, claim.Capture.RecordingID)
			} else if state == "deadline" {
				_, err = db.Exec(`UPDATE recording_captures SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 second' WHERE id=$1`, claim.Capture.ID)
			} else if state != "uploading" {
				_, err = db.Exec(`UPDATE recording_captures SET state=$2,terminal_at=now(),terminal_reason='capture_incomplete' WHERE id=$1`, claim.Capture.ID, state)
			}
			if err != nil {
				t.Fatal(err)
			}
			// The old slot can be reused while the old worker finishes provider I/O.
			if _, err = db.Exec(`UPDATE recording_processing_slots SET job_id='successor-job',claim_id='successor',leased_until=now()+interval '2 minutes' WHERE claim_id=$1`, claim.ClaimID); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				terminal, err := store.FinishTerminalLiveClaim(ctx, claim)
				if err != nil || terminal != (state != "uploading") {
					t.Fatalf("terminal cleanup: %v %v", terminal, err)
				}
			}
			var successor, finished, released bool
			if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM recording_processing_slots WHERE job_id='successor-job' AND claim_id='successor')`).Scan(&successor); err != nil || !successor {
				t.Fatalf("successor slot changed: %v %v", successor, err)
			}
			if err := db.QueryRow(`SELECT finished_at IS NOT NULL FROM recording_live_attempts WHERE claim_id=$1`, claim.ClaimID).Scan(&finished); err != nil || finished != (state != "uploading") {
				t.Fatalf("attempt completion: %v %v", finished, err)
			}
			if err := db.QueryRow(`SELECT claim_id IS NULL FROM recording_live WHERE capture_id=$1`, claim.Capture.ID).Scan(&released); err != nil || released != (state != "uploading") {
				t.Fatalf("live claim cleanup: %v %v", released, err)
			}
			if state != "uploading" {
				if _, err := db.Exec(`UPDATE recording_live SET claim_id='successor',claimed_until=now()+interval '2 minutes' WHERE capture_id=$1`, claim.Capture.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.FinishTerminalLiveClaim(ctx, claim); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT claim_id='successor' FROM recording_live WHERE capture_id=$1`, claim.Capture.ID).Scan(&successor); err != nil || !successor {
					t.Fatalf("successor live claim changed: %v %v", successor, err)
				}
			}
			progress, err := store.LiveProgress(ctx, claim.Capture.ID)
			if err != nil || progress.LastSequence != -1 {
				t.Fatalf("cleanup published bytes: %+v %v", progress, err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := store.FinishTerminalLiveClaim(cancelled, claim); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation hidden: %v", err)
			}
		})
	}
}

func TestTerminalLiveClaimCleanupFailureRollsBack(t *testing.T) {
	db, store, _, _ := liveWorkerFixture(t)
	ctx := context.Background()
	claim, err := store.ClaimLiveValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_captures SET state='aborted',terminal_at=now() WHERE id=$1`, claim.Capture.ID); err != nil {
		t.Fatal(err)
	}
	// Fail the final slot update after live/attempt cleanup has been attempted.
	if _, err := db.Exec(`ALTER TABLE recording_processing_slots ADD CONSTRAINT test_reject_release CHECK (claim_id IS NOT NULL) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if terminal, err := store.FinishTerminalLiveClaim(ctx, claim); terminal || err == nil {
		t.Fatalf("cleanup failure hidden: %v %v", terminal, err)
	}
	var intact bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM recording_live WHERE capture_id=$1 AND claim_id=$2) AND EXISTS(SELECT 1 FROM recording_live_attempts WHERE claim_id=$2 AND finished_at IS NULL) AND EXISTS(SELECT 1 FROM recording_processing_slots WHERE claim_id=$2)`, claim.Capture.ID, claim.ClaimID).Scan(&intact); err != nil || !intact {
		t.Fatalf("cleanup partially committed: %v %v", intact, err)
	}
}

func TestTerminalLiveClaimCleanupChecksPackageAuthority(t *testing.T) {
	for _, state := range []string{"uploading", "failed", "expired"} {
		t.Run(state, func(t *testing.T) {
			db, store, _, _ := liveWorkerFixture(t)
			ctx := context.Background()
			claim, err := store.ClaimLiveValidation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO recording_packages(id,session_id,owner_service,actor_id,recording_id,idempotency_key,state,size_bytes,inventory,created_at,expires_at) SELECT id,id,'hhc-web-api',actor_id,recording_id,'package:'||id,$2,declared_bytes,'{}',created_at,expires_at FROM recording_captures WHERE id=$1`, claim.Capture.ID, state); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE recording_captures SET package_id=id WHERE id=$1`, claim.Capture.ID); err != nil {
				t.Fatal(err)
			}
			terminal, err := store.FinishTerminalLiveClaim(ctx, claim)
			if err != nil || terminal != (state != "uploading") {
				t.Fatalf("package authority ignored: %v %v", terminal, err)
			}
		})
	}
}
