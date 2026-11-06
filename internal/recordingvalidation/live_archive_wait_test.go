package recordingvalidation

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

type archiveSnapshots struct {
	values []postgres.RecordingLiveSnapshot
	err    error
	calls  int
}

func (s *archiveSnapshots) LiveArchiveSnapshot(context.Context, string) (postgres.RecordingLiveSnapshot, error) {
	s.calls++
	if s.err != nil {
		return postgres.RecordingLiveSnapshot{}, s.err
	}
	index := min(s.calls-1, len(s.values)-1)
	return s.values[index], nil
}
func TestArchiveWaitRequiresPublishedEndAndIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		segment := assets.RecordingLiveSegment{Renditions: map[string]assets.RecordingLiveFragment{"720p": {Validation: &assets.RecordingLiveValidation{Version: liveValidationVersion, SegmentETag: "version"}}}}
		ready := postgres.RecordingLiveSnapshot{Segments: []assets.RecordingLiveSegment{segment}, Revision: 2, PublishedRevision: 2, Ended: true}
		unpublished := ready
		unpublished.PublishedRevision = 1
		store := &archiveSnapshots{values: []postgres.RecordingLiveSnapshot{unpublished, ready}}
		got, err := waitForLiveArchive(context.Background(), store, "capture")
		if err != nil || len(got) != 1 || store.calls != 2 {
			t.Fatalf("unpublished evidence reused: %v %d", err, store.calls)
		}
		store = &archiveSnapshots{values: []postgres.RecordingLiveSnapshot{{}}}
		started := time.Now()
		got, err = waitForLiveArchive(context.Background(), store, "capture")
		if err != nil || got != nil || time.Since(started) != time.Minute {
			t.Fatalf("unbounded wait: %v %v", err, time.Since(started))
		}
	})
}
func TestArchiveWaitLegacyMissingAndCancellation(t *testing.T) {
	for _, store := range []*archiveSnapshots{
		{err: assets.ErrNotFound},
		{values: []postgres.RecordingLiveSnapshot{{Segments: []assets.RecordingLiveSegment{{Renditions: map[string]assets.RecordingLiveFragment{"720p": {}}}}}}},
	} {
		got, err := waitForLiveArchive(context.Background(), store, "capture")
		if err != nil || got != nil || store.calls != 1 {
			t.Fatalf("legacy fallback: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitForLiveArchive(ctx, &archiveSnapshots{values: []postgres.RecordingLiveSnapshot{{}}}, "capture")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type blockedArchiveSnapshot struct{}

func (blockedArchiveSnapshot) LiveArchiveSnapshot(ctx context.Context, _ string) (postgres.RecordingLiveSnapshot, error) {
	<-ctx.Done()
	return postgres.RecordingLiveSnapshot{}, ctx.Err()
}
func TestArchiveWaitBoundsTheDatabaseCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		got, err := waitForLiveArchive(ctx, blockedArchiveSnapshot{}, "capture")
		if got != nil || err != nil || time.Since(start) != time.Minute {
			t.Fatalf("DB wait escaped bound: %v %v", err, time.Since(start))
		}
	})
}
