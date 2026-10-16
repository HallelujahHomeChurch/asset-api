package recordingvalidation

import (
	"context"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAttemptTimingConcurrentWorkAndSafeFailure(t *testing.T) {
	tracker := NewAttemptTiming("package_validation")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tracker.Stage("decode", 2*time.Second) }()
	}
	wg.Wait()
	tracker.Finish(packageStageError("object_read", errors.New("https://private.invalid/?secret=hidden")))
	snapshot := tracker.Snapshot()
	if snapshot.Stages["decode"].ElapsedMS != 20000 || snapshot.Stages["decode"].Calls != 10 {
		t.Fatalf("lost concurrent work: %+v", snapshot)
	}
	if snapshot.FailureStage != "object_read" || snapshot.FailureReason != "transient" || snapshot.WorkFinishedAt == nil {
		t.Fatalf("lost diagnostic: %+v", snapshot)
	}
	bytes, _ := json.Marshal(snapshot)
	if strings.Contains(string(bytes), "hidden") || strings.Contains(string(bytes), "private.invalid") {
		t.Fatal("persisted raw error")
	}
	tracker.Stage("private-url-secret", time.Second)
	if _, ok := tracker.Snapshot().Stages["private-url-secret"]; ok {
		t.Fatal("unbounded stage names persisted")
	}
}

func TestAttemptTimingPhaseSnapshotDoesNotDoubleCount(t *testing.T) {
	tracker := NewAttemptTiming("source_finalization")
	tracker.phaseStarted = tracker.phaseStarted.Add(-time.Second)
	tracker.Phase("encoding")
	first := tracker.Snapshot()
	second := tracker.Snapshot()
	if first.Phases["source_finalization"] < 1000 || second.Phases["source_finalization"] != first.Phases["source_finalization"] {
		t.Fatal("closed phase changed")
	}
	tracker.Finish(nil)
	first = tracker.Snapshot()
	second = tracker.Snapshot()
	if first.ElapsedMS != second.ElapsedMS || first.Phases["encoding"] != second.Phases["encoding"] {
		t.Fatal("completed timing kept running")
	}
}

func TestAttemptTimingFlushFailureLeavesCompletedWork(t *testing.T) {
	tracker := NewAttemptTiming("package_validation")
	tracker.Stage("decode", time.Second)
	tracker.Finish(nil)
	tracker.Flush(context.Background(), func(ctx context.Context, value assets.RecordingAttemptDiagnostics) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("diagnostic write is not bounded")
		}
		return errors.New("private connection detail")
	})
	value := tracker.Snapshot()
	if value.FailureReason != "none" || value.WorkFinishedAt == nil || value.Stages["decode"].ElapsedMS != 1000 {
		t.Fatal("telemetry failure changed media result")
	}
}
