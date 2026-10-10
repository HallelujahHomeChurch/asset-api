package assets

import (
	"errors"
	"math"
	"testing"
)

func intBoundary(n int) *int { return &n }

func TestStaleEpochCannotAdvanceRange(t *testing.T) {
	current := RecordingBroadcastRange{MemberState: "blocked", RecordingID: "11111111-1111-4111-8111-111111111111", Epoch: 2, RangeRevision: 3, StartSequence: intBoundary(8)}
	stale := current
	stale.Epoch = 1
	stale.RangeRevision = 4
	if err := ValidateBroadcastRangeChange(&current, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale epoch: %v", err)
	}
	next := current
	next.RangeRevision++
	next.EndSequenceExclusive = intBoundary(20)
	if err := ValidateBroadcastRangeChange(&current, next); err != nil {
		t.Fatal(err)
	}
	frozen := next
	frozen.RangeRevision++
	frozen.EndSequenceExclusive = intBoundary(21)
	if err := ValidateBroadcastRangeChange(&next, frozen); !errors.Is(err, ErrConflict) {
		t.Fatalf("expanded frozen end: %v", err)
	}
	frozen = next
	frozen.RangeRevision++
	frozen.EndSequenceExclusive = nil
	if err := ValidateBroadcastRangeChange(&next, frozen); !errors.Is(err, ErrConflict) {
		t.Fatalf("removed frozen end: %v", err)
	}
}

func TestBroadcastProjectionPreservesRawTimeline(t *testing.T) {
	var history []RecordingLiveSegment
	for i := 0; i < 24; i++ {
		batch := liveBatch(float64(i)*30.03, float64(i+1)*30.03)
		for name, v := range batch {
			v.FrameRate = 30000.0 / 1001
			batch[name] = v
		}
		var err error
		history, err = AppendLiveSegment(history, batch, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	policy := RecordingBroadcastRange{MemberState: "blocked", Epoch: 1, RangeRevision: 3, StartSequence: intBoundary(8), EndSequenceExclusive: intBoundary(20)}
	projection := BroadcastProjection(policy, history, 24, true)
	if projection.State != "ready" || projection.DurationSeconds == nil || math.Abs(*projection.DurationSeconds-360.36) > 1e-6 || projection.MediaStartSeconds == nil || math.Abs(*projection.MediaStartSeconds-240.24) > 1e-6 {
		t.Fatalf("projection: %+v", projection)
	}
	if len(history) != 24 || history[0].Sequence != 0 || history[23].Sequence != 23 {
		t.Fatal("raw history changed")
	}
	policy.EndSequenceExclusive = intBoundary(25)
	if got := BroadcastProjection(policy, history, 24, true); got.State != "failed" {
		t.Fatalf("invalid sealed marker: %+v", got)
	}
	if got := BroadcastProjection(policy, history, 24, false); got.State != "pending" {
		t.Fatalf("future marker: %+v", got)
	}
}
