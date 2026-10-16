package recordingvalidation

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"hhc/asset-api/internal/assets"
)

// Periodic bounded snapshots survive retries without one DB write per fragment.
type AttemptTiming struct {
	mu           sync.Mutex
	flushMu      sync.Mutex
	started      time.Time
	phaseStarted time.Time
	phase        string
	value        assets.RecordingAttemptDiagnostics
	durations    map[string]time.Duration
}

func NewAttemptTiming(phase string) *AttemptTiming {
	now := time.Now()
	return &AttemptTiming{started: now, phaseStarted: now, phase: phase, durations: map[string]time.Duration{}, value: assets.RecordingAttemptDiagnostics{Phases: map[string]int64{}, Stages: map[string]assets.RecordingStageTiming{}, FailureStage: "none", FailureReason: "none"}}
}

func (t *AttemptTiming) Stage(stage string, elapsed time.Duration) {
	switch stage {
	case "freeze_hash", "probe", "decode", "inventory", "playlists", "inventory_write", "object_head_staging", "object_copy", "object_head_final", "object_download_hash":
	default:
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.durations[stage] += elapsed
	value := t.value.Stages[stage]
	value.ElapsedMS = t.durations[stage].Milliseconds()
	value.Calls++
	t.value.Stages[stage] = value
}

func (t *AttemptTiming) Phase(phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if phase == t.phase || t.value.WorkFinishedAt != nil {
		return
	}
	now := time.Now()
	t.value.Phases[t.phase] += now.Sub(t.phaseStarted).Milliseconds()
	t.phase, t.phaseStarted = phase, now
}

func (t *AttemptTiming) Finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value.WorkFinishedAt != nil {
		return
	}
	now := time.Now()
	t.value.Phases[t.phase] += now.Sub(t.phaseStarted).Milliseconds()
	t.value.ElapsedMS = now.Sub(t.started).Milliseconds()
	finished := now.UTC()
	t.value.WorkFinishedAt = &finished
	t.value.FailureStage, t.value.FailureReason = packageFailureDiagnostic(err)
}

func (t *AttemptTiming) Snapshot() assets.RecordingAttemptDiagnostics {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	value := t.value
	value.ObservedAt = now.UTC()
	value.Phases = maps.Clone(value.Phases)
	value.Stages = maps.Clone(value.Stages)
	if value.WorkFinishedAt == nil {
		value.ElapsedMS = now.Sub(t.started).Milliseconds()
		value.Phases[t.phase] += now.Sub(t.phaseStarted).Milliseconds()
	}
	return value
}

// Diagnostics cannot turn successful media validation into a failed upload.
func (t *AttemptTiming) Flush(ctx context.Context, write func(context.Context, assets.RecordingAttemptDiagnostics) error) {
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := write(writeCtx, t.Snapshot()); err != nil {
		_, reason := packageFailureDiagnostic(err)
		slog.Warn("recording_attempt_diagnostics_deferred", "reason", reason)
	}
}
