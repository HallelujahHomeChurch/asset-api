package recordingvalidation

import (
	"context"
	"sync"
	"time"

	"hhc/asset-api/internal/assets"
)

// The worker timer owns persistence. Reports only replace a memory snapshot;
// they never write one database row per fragment or extend a lease.
type ProcessingProgressTracker struct {
	mu    sync.Mutex
	value assets.RecordingProcessingProgress
}

func NewProcessingProgressTracker(initial assets.RecordingProcessingProgress) *ProcessingProgressTracker {
	return &ProcessingProgressTracker{value: initial}
}

func (p *ProcessingProgressTracker) Report(value assets.RecordingProcessingProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value.Attempt = p.value.Attempt
	value.AttemptStartedAt = p.value.AttemptStartedAt
	if value.Phase == p.value.Phase {
		value.PhaseStartedAt = p.value.PhaseStartedAt
	}
	if value.LastProgressAt.Before(p.value.LastProgressAt) {
		value.LastProgressAt = p.value.LastProgressAt
	}
	if value.PhaseStartedAt.Before(p.value.PhaseStartedAt) {
		value.PhaseStartedAt = p.value.PhaseStartedAt
	}
	value.HeartbeatAt = value.LastProgressAt
	p.value = value
}

func (p *ProcessingProgressTracker) Phase(phase string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if at.Before(p.value.LastProgressAt) {
		at = p.value.LastProgressAt
	}
	if p.value.Phase != phase {
		p.value.Phase = phase
		p.value.PhaseStartedAt = at
		p.value.LastProgressAt = at
		p.value.HeartbeatAt = at
	}
}

func (p *ProcessingProgressTracker) Snapshot(at time.Time) assets.RecordingProcessingProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := p.value
	if at.After(value.HeartbeatAt) {
		value.HeartbeatAt = at
	}
	return value
}

// Serialize phase/terminal flushes with the heartbeat so an older snapshot
// cannot arrive after a newer phase and cancel an otherwise healthy claim.
func (p *ProcessingProgressTracker) Flush(ctx context.Context, at time.Time, write func(context.Context, assets.RecordingProcessingProgress) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := p.value
	if at.After(value.HeartbeatAt) {
		value.HeartbeatAt = at
	}
	return write(ctx, value)
}
