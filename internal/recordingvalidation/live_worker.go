package recordingvalidation

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/storage/r2"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type LiveObjects interface {
	PackageObjects
	PublishLiveObject(context.Context, string, string, string) (string, error)
	PutLivePlaylist(context.Context, string, int64, string, []byte) error
	AdvanceLivePointer(context.Context, string, int64, int) error
}

const liveAdmissionTimeout = 5 * time.Second

// ClaimLiveWork reserves one fenced slot. A nil function means no work is ready.
func ClaimLiveWork(ctx context.Context, store *postgres.RecordingCaptureStore, objects LiveObjects, probe PackageMediaProbe) (func() error, error) {
	admit, cancel := context.WithTimeout(ctx, liveAdmissionTimeout)
	defer cancel()
	claim, err := store.ClaimLiveValidation(admit)
	if err != nil && admit.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		slog.Warn("recording_live_admission_timeout", "operation", "segment")
		return nil, nil
	}
	if errors.Is(err, assets.ErrNotFound) {
		return claimLiveRepair(ctx, store, objects, probe)
	}
	if err != nil {
		return nil, err
	}
	return func() error { return runLiveClaim(ctx, store, objects, probe, claim) }, nil
}

// Media bytes never become reachable through the common pointer before checks.
func runLiveClaim(ctx context.Context, store *postgres.RecordingCaptureStore, objects LiveObjects, probe PackageMediaProbe, claim postgres.RecordingLiveClaim) error {
	snapshot := claim.Snapshot
	claimedAt := time.Now()
	slog.Info("recording_live_claim_started", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence)
	err := RunProcessingClaim(ctx, func(ctx context.Context) error { return store.HeartbeatLive(ctx, claim) }, func(ctx context.Context) (result error) {
		const publicationStageTimeout = 20 * time.Second
		stage, started := "resume_publication", time.Now()
		var times liveCopyTiming
		var commitDuration time.Duration
		logStage := func(success bool) {
			slog.Info("recording_live_stage", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence,
				"segment_frames", claim.Capture.SegmentFrames, "stage", stage, "elapsed_ms", time.Since(started).Milliseconds(), "succeeded", success,
				"head_max_ms", times.head.Milliseconds(), "head_local_timeouts", times.headTimeouts, "head_local_retries", times.headRetries, "head_http_attempts", times.headSends, "copy_max_ms", times.copy.Milliseconds(), "batch_commit_ms", commitDuration.Milliseconds())
		}
		defer func() { logStage(result == nil) }()
		if snapshot.Revision == snapshot.PublishedRevision {
			if claim.EndOnly {
				endCtx, cancel := context.WithTimeout(ctx, publicationStageTimeout)
				defer cancel()
				stage = "commit_end"
				var err error
				snapshot, err = store.CommitLiveEnd(endCtx, claim)
				if err != nil {
					return err
				}
			} else {
				stage = "validation"
				probe.LiveInitEvidence = make(map[string]assets.RecordingLiveValidation)
				if len(snapshot.Segments) > 0 {
					for name, fragment := range snapshot.Segments[0].Renditions {
						if fragment.Validation != nil {
							probe.LiveInitEvidence[name] = *fragment.Validation
						}
					}
				}
				probe.allowLiveGaps = len(snapshot.Segments) > 0
				batch, err := probe.ValidateLiveSegment(ctx, claim.Capture.ID, claim.ClaimID, claim.Sequence, claim.Objects)
				if err != nil {
					return err
				}
				// Validate continuity before publishing any stable final object.
				normalTail := claim.Capture.Inventory != nil && len(claim.Capture.Inventory.Renditions) == 3
				if normalTail {
					for _, r := range claim.Capture.Inventory.Renditions {
						if r.SegmentCount != claim.Sequence+1 {
							normalTail = false
						}
					}
				}
				if _, err = assets.AppendAvailableLiveSegment(snapshot.Segments, batch, normalTail, claim.Capture.SegmentFrames); err != nil {
					slog.Info("recording_live_boundary", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence,
						"sealed_snapshot", claim.Capture.Inventory != nil, "normal_tail", normalTail, "waiting_for_seal", errors.Is(err, assets.ErrCaptureMissingObjects))
					if claim.Capture.Inventory != nil && errors.Is(err, assets.ErrCaptureMissingObjects) {
						return assets.ErrInvalidUpload
					}
					return err
				}
				logStage(true)
				stage, started = "stable_publication", time.Now()
				stableCtx, cancelStable := context.WithTimeout(ctx, publicationStageTimeout)
				defer cancelStable()
				versions, err := publishLiveRenditions(stableCtx, objects, claim, batch, &times)
				if err != nil {
					return err
				}
				for _, fragment := range batch {
					if fragment.Validation != nil {
						fragment.Validation.InitETag = versions[fragment.Validation.Init.Path]
						fragment.Validation.SegmentETag = versions[fragment.Validation.Segment.Path]
					}
				}
				commitStarted := time.Now()
				snapshot, err = store.CommitLiveBatch(stableCtx, claim, batch)
				commitDuration = time.Since(commitStarted)
				if err != nil {
					return err
				}
			}
		}
		logStage(true)
		playlistCtx, cancel := context.WithTimeout(ctx, publicationStageTimeout)
		defer cancel()
		stage, started = "playlists", time.Now()
		playlists, err := assets.RecordingLivePlaylists(snapshot.Segments, snapshot.Ended, claim.Capture.SegmentFrames)
		if err != nil {
			return err
		}
		if err := publishLivePlaylists(playlistCtx, objects, claim.Capture.ID, snapshot.Revision, playlists); err != nil {
			return err
		}
		logStage(true)
		stage, started = "pointer", time.Now()
		return objects.AdvanceLivePointer(playlistCtx, claim.Capture.ID, snapshot.Revision, len(snapshot.Segments)-1)
	}, func(ctx context.Context, ready bool, failure string) error {
		finishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		finishStarted := time.Now()
		var finishErr error
		if ready {
			finishErr = store.FinishLivePublication(finishCtx, claim, snapshot.Revision)
		} else {
			finishErr = store.FailLiveClaim(finishCtx, claim, failure == "invalid")
		}
		slog.Info("recording_live_claim", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence,
			"revision", snapshot.Revision, "snapshot_last_sequence", len(snapshot.Segments)-1, "finish_elapsed_ms", time.Since(finishStarted).Milliseconds(), "elapsed_ms", time.Since(claimedAt).Milliseconds(), "published", ready && finishErr == nil, "failure", failure)
		return finishErr
	}, 30*time.Second)
	if errors.Is(err, assets.ErrConflict) && ctx.Err() == nil {
		cleanupCtx, cancel := context.WithTimeout(ctx, liveAdmissionTimeout)
		defer cancel()
		terminal, cleanupErr := store.FinishTerminalLiveClaim(cleanupCtx, claim)
		if cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		if terminal {
			slog.Info("recording_live_claim_cancelled", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence, "reason", "capture_terminal")
			return nil
		}
	}
	return err
}

type liveCopyTiming struct {
	head, copy                time.Duration
	headTimeouts, headRetries int
	headSends                 int64
}

const livePublicationHeadTimeout = 2 * time.Second

// HEAD is read-only. Retry only our own expired call, while the batch is
// still active. If the SDK already retried, leave recovery to the claim.
func headForLiveCopy(ctx context.Context, objects LiveObjects, key string, timing *liveCopyTiming) (int64, string, error) {
	var sends atomic.Int64
	counted := r2.WithAttemptCounter(ctx, &sends)
	started := time.Now()
	defer func() {
		timing.head = time.Since(started)
		timing.headSends = sends.Load()
	}()
	for {
		child, cancel := context.WithTimeout(counted, livePublicationHeadTimeout)
		size, etag, err := objects.Head(child, key)
		localTimeout := child.Err() == context.DeadlineExceeded && errors.Is(err, context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		if !localTimeout {
			return size, etag, err
		}
		timing.headTimeouts++
		deadline, bounded := ctx.Deadline()
		// Leave at least one call budget for the conditional copy and commit.
		if timing.headRetries > 0 || sends.Load() > 1 || (bounded && time.Until(deadline) <= 2*livePublicationHeadTimeout) {
			return 0, "", err
		}
		timing.headRetries++
	}
}

// Once the initial profiles are established, one storage failure must not
// cancel independent renditions. Keep time for the fenced database commit.
func publishLiveRenditions(ctx context.Context, objects LiveObjects, claim postgres.RecordingLiveClaim, batch map[string]assets.RecordingLiveFragment, timing *liveCopyTiming) (map[string]string, error) {
	if claim.Sequence == 0 {
		return copyLiveObjects(ctx, objects, claim, batch, timing)
	}
	type outcome struct {
		name     string
		versions map[string]string
		timing   liveCopyTiming
		err      error
	}
	out := make(chan outcome, len(batch))
	count := len(batch)
	for name, fragment := range batch {
		go func() {
			child, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			var result outcome
			result.name = name
			result.versions, result.err = copyLiveObjects(child, objects, claim, map[string]assets.RecordingLiveFragment{name: fragment}, &result.timing)
			out <- result
		}()
	}
	versions := make(map[string]string)
	var failed error
	for range count {
		res := <-out
		timing.head = max(timing.head, res.timing.head)
		timing.copy = max(timing.copy, res.timing.copy)
		timing.headRetries += res.timing.headRetries
		timing.headTimeouts += res.timing.headTimeouts
		timing.headSends += res.timing.headSends
		if res.err != nil {
			delete(batch, res.name)
			failed = errors.Join(failed, res.err)
			continue
		}
		for key, version := range res.versions {
			versions[key] = version
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(batch) == 0 {
		return nil, failed
	}
	return versions, nil
}

// Join bounded provider copies before committing the batch or releasing its lease.
func copyLiveObjects(ctx context.Context, objects LiveObjects, claim postgres.RecordingLiveClaim, batch map[string]assets.RecordingLiveFragment, timing *liveCopyTiming) (map[string]string, error) {
	versions := make(map[string]string, len(claim.Objects))
	pending := make([]assets.RecordingPackageObject, 0, len(claim.Objects))
	for _, object := range claim.Objects {
		rendition, _, _ := strings.Cut(object.Path, "/")
		if _, ok := batch[rendition]; batch != nil && !ok {
			continue
		}
		reused := false
		for _, fragment := range batch {
			receipt := fragment.Validation
			if receipt != nil && receipt.Init == object && receipt.InitETag != "" {
				versions[object.Path] = receipt.InitETag
				reused = true
				break
			}
		}
		if !reused {
			pending = append(pending, object)
		}
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	from := "recordings/packages/" + claim.Capture.ID + "/final/" + claim.ClaimID + "/"
	to := "recordings/captures/" + claim.Capture.ID + "/final/"
	var workers sync.WaitGroup
	var mu sync.Mutex
	var result error
	for worker := 0; worker < 3; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := worker; index < len(pending); index += 3 {
				if workCtx.Err() != nil {
					return
				}
				object := pending[index]
				var measured liveCopyTiming
				rendition, _, _ := strings.Cut(object.Path, "/")
				transferCtx := r2.WithLiveAttempt(workCtx, claim.Capture.ID, claim.ClaimID, rendition, "stable", claim.Sequence)
				size, etag, err := headForLiveCopy(transferCtx, objects, from+object.Path, &measured)
				mu.Lock()
				timing.head = max(timing.head, measured.head)
				timing.headRetries += measured.headRetries
				timing.headTimeouts += measured.headTimeouts
				timing.headSends += measured.headSends
				mu.Unlock()
				if err == nil && size != object.SizeBytes {
					err = assets.ErrInvalidUpload
				}
				if err == nil {
					var published string
					copyStarted := time.Now()
					published, err = objects.PublishLiveObject(transferCtx, from+object.Path, to+object.Path, etag)
					mu.Lock()
					timing.copy = max(timing.copy, time.Since(copyStarted))
					mu.Unlock()
					if err == nil && published == "" {
						err = assets.ErrInvalidUpload
					}
					if err == nil {
						mu.Lock()
						versions[object.Path] = published
						mu.Unlock()
					}
				}
				if err != nil {
					mu.Lock()
					if result == nil {
						result = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
	workers.Wait()
	if result != nil {
		return nil, result
	}
	return versions, ctx.Err()
}

// All four immutable playlists must finish before the common pointer advances.
func publishLivePlaylists(ctx context.Context, objects LiveObjects, capture string, revision int64, playlists map[string][]byte) error {
	if len(playlists) == 0 || len(playlists) > 4 {
		return assets.ErrInvalidInput
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(playlists))
	for name, data := range playlists {
		go func() {
			err := objects.PutLivePlaylist(workCtx, capture, revision, name, data)
			if err != nil {
				cancel()
			}
			results <- err
		}()
	}
	var result error
	for range playlists {
		result = errors.Join(result, <-results)
	}
	if result != nil {
		return result
	}
	return ctx.Err()
}
