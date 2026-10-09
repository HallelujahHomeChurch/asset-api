package recordingvalidation

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"log/slog"
	"time"
)

type LiveObjects interface {
	PackageObjects
	PublishLiveObject(context.Context, string, string, string) error
	PutLivePlaylist(context.Context, string, int64, string, []byte) error
	AdvanceLivePointer(context.Context, string, int64, int) error
}

// Drain both global slots across captures; each capture still advances serially.
// Join every claimed batch before the caller starts cleanup or long VOD work.
func RunLiveValidation(ctx context.Context, store *postgres.RecordingCaptureStore, objects LiveObjects, probe PackageMediaProbe) (bool, error) {
	done := make(chan error, 2)
	count := 0
	var result error
	for count < 2 {
		claim, err := store.ClaimLiveValidation(ctx)
		if errors.Is(err, assets.ErrNotFound) {
			break
		}
		if err != nil {
			result = err
			break
		}
		count++
		go func() { done <- runLiveClaim(ctx, store, objects, probe, claim) }()
	}
	for i := 0; i < count; i++ {
		result = errors.Join(result, <-done)
	}
	return count > 0, result
}

// Media bytes never become reachable through the common pointer before checks.
func runLiveClaim(ctx context.Context, store *postgres.RecordingCaptureStore, objects LiveObjects, probe PackageMediaProbe, claim postgres.RecordingLiveClaim) error {
	snapshot := claim.Snapshot
	claimedAt := time.Now()
	err := RunProcessingClaim(ctx, func(ctx context.Context) error { return store.HeartbeatLive(ctx, claim) }, func(ctx context.Context) (result error) {
		stage, started := "resume_publication", time.Now()
		logStage := func(success bool) {
			slog.Info("recording_live_stage", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence,
				"stage", stage, "elapsed_ms", time.Since(started).Milliseconds(), "succeeded", success)
		}
		defer func() { logStage(result == nil) }()
		if snapshot.Revision == snapshot.PublishedRevision {
			if claim.EndOnly {
				stage = "commit_end"
				var err error
				snapshot, err = store.CommitLiveEnd(ctx, claim)
				if err != nil {
					return err
				}
			} else {
				stage = "validation"
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
				if _, err = assets.AppendLiveSegment(snapshot.Segments, batch, normalTail); err != nil {
					return err
				}
				logStage(true)
				stage, started = "stable_publication", time.Now()
				from := "recordings/packages/" + claim.Capture.ID + "/final/" + claim.ClaimID + "/"
				to := "recordings/captures/" + claim.Capture.ID + "/final/"
				for _, object := range claim.Objects {
					size, etag, err := objects.Head(ctx, from+object.Path)
					if err != nil {
						return err
					}
					if size != object.SizeBytes {
						return assets.ErrInvalidUpload
					}
					if err = objects.PublishLiveObject(ctx, from+object.Path, to+object.Path, etag); err != nil {
						return err
					}
				}
				snapshot, err = store.CommitLiveBatch(ctx, claim, batch)
				if err != nil {
					return err
				}
			}
		}
		logStage(true)
		stage, started = "playlists", time.Now()
		playlists, err := assets.RecordingLivePlaylists(snapshot.Segments, snapshot.Ended)
		if err != nil {
			return err
		}
		for name, data := range playlists {
			if err := objects.PutLivePlaylist(ctx, claim.Capture.ID, snapshot.Revision, name, data); err != nil {
				return err
			}
		}
		logStage(true)
		stage, started = "pointer", time.Now()
		return objects.AdvanceLivePointer(ctx, claim.Capture.ID, snapshot.Revision, len(snapshot.Segments)-1)
	}, func(ctx context.Context, ready bool, failure string) error {
		var finishErr error
		if ready {
			finishErr = store.FinishLivePublication(ctx, claim, snapshot.Revision)
		} else {
			finishErr = store.FailLiveClaim(ctx, claim, failure == "invalid")
		}
		slog.Info("recording_live_claim", "capture_id", claim.Capture.ID, "claim_id", claim.ClaimID, "sequence", claim.Sequence,
			"elapsed_ms", time.Since(claimedAt).Milliseconds(), "published", ready && finishErr == nil, "failure", failure)
		return finishErr
	}, 30*time.Second)
	return err
}
