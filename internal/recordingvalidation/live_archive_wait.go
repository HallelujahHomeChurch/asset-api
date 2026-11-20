package recordingvalidation

import (
	"context"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

// Seal may arrive before the final live batch publishes. Bound this wait while
// the existing package heartbeat keeps its lease; legacy evidence falls back.
func waitForLiveArchive(ctx context.Context, store interface {
	LiveArchiveSnapshot(context.Context, string) (postgres.RecordingLiveSnapshot, error)
}, id string) ([]assets.RecordingLiveSegment, string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		snapshot, err := store.LiveArchiveSnapshot(waitCtx, id)
		if ctx.Err() != nil {
			return nil, "canceled", ctx.Err()
		}
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return nil, "wait_timeout", nil
		}
		if errors.Is(err, assets.ErrNotFound) {
			return nil, "capture_missing", nil
		}
		if err != nil {
			return nil, "snapshot_error", err
		}
		if snapshot.Ended && snapshot.Revision == snapshot.PublishedRevision {
			return snapshot.Segments, "published_end", nil
		}
		if len(snapshot.Segments) > 0 {
			for _, fragment := range snapshot.Segments[0].Renditions {
				if fragment.Validation == nil || fragment.Validation.Version != liveValidationVersion || fragment.Validation.SegmentETag == "" {
					return nil, "legacy_receipt", nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, "canceled", ctx.Err()
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, "canceled", ctx.Err()
			}
			return nil, "wait_timeout", nil
		case <-tick.C:
		}
	}
}
