package recordingvalidation

import (
	"context"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestProcessingProgressFlushSerializesPhaseChanges(t *testing.T) {
	start := time.Now().UTC()
	tracker := NewProcessingProgressTracker(assets.RecordingProcessingProgress{Attempt: 1, Phase: "package_validation", AttemptStartedAt: start, PhaseStartedAt: start, LastProgressAt: start, HeartbeatAt: start})
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		_ = tracker.Flush(context.Background(), start, func(_ context.Context, p assets.RecordingProcessingProgress) error {
			close(entered)
			<-release
			if p.Phase != "package_validation" {
				t.Errorf("snapshot changed while writing: %s", p.Phase)
			}
			return nil
		})
	}()
	<-entered
	phaseDone := make(chan struct{})
	go func() { tracker.Phase("package_finalization", start.Add(time.Second)); close(phaseDone) }()
	close(release)
	<-finished
	<-phaseDone
	if err := tracker.Flush(context.Background(), start, func(_ context.Context, p assets.RecordingProcessingProgress) error {
		if p.Phase != "package_finalization" {
			t.Fatalf("new phase lost: %+v", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProcessingProgressTrackerKeepsActualProgressClock(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	initial := assets.RecordingProcessingProgress{Attempt: 2, Phase: "package_validation", AttemptStartedAt: start, PhaseStartedAt: start, LastProgressAt: start, HeartbeatAt: start}
	tracker := NewProcessingProgressTracker(initial)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := 0; n < 20; n++ {
				tracker.Snapshot(start.Add(time.Minute))
			}
		}()
	}
	workers.Wait()
	if got := tracker.Snapshot(start.Add(time.Minute)); !got.LastProgressAt.Equal(start) || !got.HeartbeatAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("heartbeat manufactured progress: %+v", got)
	}
	one := int64(1)
	report := assets.RecordingProcessingProgress{Phase: "package_validation", Rendition: "720p", ObjectsVerified: &one, LastProgressAt: start.Add(2 * time.Minute), PhaseStartedAt: start}
	tracker.Report(report)
	got := tracker.Snapshot(start.Add(3 * time.Minute))
	if got.Attempt != 2 || !got.AttemptStartedAt.Equal(start) || !got.LastProgressAt.Equal(report.LastProgressAt) || got.ObjectsVerified == nil || *got.ObjectsVerified != 1 {
		t.Fatalf("claim progress lost: %+v", got)
	}
	tracker.Phase("package_finalization", start)
	if got := tracker.Snapshot(start); assets.ValidateRecordingProcessingProgress(got) != nil || !got.LastProgressAt.Equal(report.LastProgressAt) || !got.PhaseStartedAt.Equal(report.LastProgressAt) {
		t.Fatalf("wall clock rollback broke summary: %+v", got)
	}
}
