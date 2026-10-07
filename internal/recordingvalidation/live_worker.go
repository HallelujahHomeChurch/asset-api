package recordingvalidation

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"time"
)

type LiveObjects interface {
	PackageObjects
	PublishLiveObject(context.Context, string, string, string) error
	PutLivePlaylist(context.Context, string, int64, string, []byte) error
	AdvanceLivePointer(context.Context, string, int64, int) error
}

// One claim validates one batch or resumes an already committed publication.
// Media bytes never become reachable through the common pointer before checks.
func RunLiveValidation(ctx context.Context, store *postgres.RecordingCaptureStore, objects LiveObjects, probe PackageMediaProbe) (bool, error) {
	claim, err := store.ClaimLiveValidation(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	snapshot := claim.Snapshot
	err = RunProcessingClaim(ctx, func(ctx context.Context) error { return store.HeartbeatLive(ctx, claim) }, func(ctx context.Context) error {
		if snapshot.Revision == snapshot.PublishedRevision {
			if claim.EndOnly {
				var err error
				snapshot, err = store.CommitLiveEnd(ctx, claim)
				if err != nil {
					return err
				}
			} else {
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
		playlists, err := assets.RecordingLivePlaylists(snapshot.Segments, snapshot.Ended)
		if err != nil {
			return err
		}
		for name, data := range playlists {
			if err := objects.PutLivePlaylist(ctx, claim.Capture.ID, snapshot.Revision, name, data); err != nil {
				return err
			}
		}
		return objects.AdvanceLivePointer(ctx, claim.Capture.ID, snapshot.Revision, len(snapshot.Segments)-1)
	}, func(ctx context.Context, ready bool, failure string) error {
		if ready {
			return store.FinishLivePublication(ctx, claim, snapshot.Revision)
		}
		return store.FailLiveClaim(ctx, claim, failure == "invalid")
	}, 30*time.Second)
	return true, err
}
