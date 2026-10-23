package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"
)

type Transition uint8

const (
	RecoveryActivated Transition = iota
	IdleCutoffCommitted
	BroadcastRevoked
)

var transitionNames = [...]string{"recovery_activated", "idle_cutoff_committed", "broadcast_revoked"}

type correlationKey struct{}
type correlation struct{ trace, parent string }

// Only the fixed W3C version00 grammar is retained, irrespective of sampling.
func WithTraceParent(ctx context.Context, value string) context.Context {
	c := correlation{}
	if len(value) == 55 && value[:3] == "00-" && value[35] == '-' && value[52] == '-' {
		raw := value[3:35] + value[36:52] + value[53:55]
		if _, err := hex.DecodeString(raw); err == nil && strings.ToLower(raw) == raw && value[3:35] != strings.Repeat("0", 32) && value[36:52] != strings.Repeat("0", 16) {
			c = correlation{value[3:35], value[36:52]}
		}
	}
	return context.WithValue(ctx, correlationKey{}, c)
}

type event struct {
	kind    Transition
	capture string
	correlation
	epoch, revision int64
	cutoff          int
	deadline        time.Time
	at              time.Time
}

// Transition is called only after a successful commit. A fixed in-memory queue
// cannot guarantee delivery across crashes or collector loss; overflow is counted.
func (r *Recorder) Transition(ctx context.Context, kind Transition, captureID string, epoch, revision int64, cutoff int, deadline time.Time) {
	if int(kind) >= len(transitionNames) {
		return
	}
	if len(captureID) != 32 || strings.ToLower(captureID) != captureID {
		return
	}
	if _, err := hex.DecodeString(captureID); err != nil {
		return
	}
	sum := sha256.Sum256([]byte("hhc.recording.capture.v1\x00" + captureID))
	c, _ := ctx.Value(correlationKey{}).(correlation)
	e := event{kind: kind, capture: hex.EncodeToString(sum[:]), correlation: c, epoch: epoch, revision: revision, cutoff: cutoff, deadline: deadline, at: time.Now().UTC()}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventCount == len(r.events) {
		r.eventStats[1]++
		return
	}
	r.events[r.eventCount] = e
	r.eventCount++
}

// Drain at most16 events per existing15-second tick; no request writes a sink.
func (r *Recorder) flushEvents(ctx context.Context, logger *slog.Logger) {
	var batch [16]event
	r.mu.Lock()
	n := min(r.eventCount, len(batch))
	copy(batch[:], r.events[:n])
	copy(r.events[:], r.events[n:r.eventCount])
	clear(r.events[r.eventCount-n : r.eventCount])
	r.eventCount -= n
	r.mu.Unlock()
	for i, e := range batch[:n] {
		if ctx.Err() != nil {
			r.countEvent(3, uint64(n-i))
			return
		}
		rec := slog.NewRecord(e.at, slog.LevelInfo, "recording_authority_committed", 0)
		rec.AddAttrs(slog.String("event", transitionNames[e.kind]), slog.String("capture_ref", e.capture), slog.Int64("epoch", e.epoch), slog.Int64("range_revision", e.revision), slog.Int("cutoff", e.cutoff))
		var traceID, parentID, deadline any
		if e.trace != "" {
			traceID = e.trace
			parentID = e.parent
		}
		if !e.deadline.IsZero() {
			deadline = e.deadline.UTC().Format(time.RFC3339Nano)
		}
		rec.AddAttrs(slog.Any("trace_id", traceID), slog.Any("parent_span_id", parentID), slog.Any("deadline", deadline))
		if !logger.Enabled(ctx, slog.LevelInfo) {
			r.countEvent(3, 1)
			continue
		}
		if err := logger.Handler().Handle(ctx, rec); err != nil {
			r.countEvent(2, 1)
		} else {
			r.countEvent(0, 1)
		}
	}
}
func (r *Recorder) countEvent(index int, n uint64) {
	r.mu.Lock()
	r.eventStats[index] += n
	r.mu.Unlock()
}
func (r *Recorder) eventSnapshot() [4]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.eventStats
	r.eventStats = [4]uint64{}
	return v
}
