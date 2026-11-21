package assets

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCaptureSignalGraceAndImmutableCutoff(t *testing.T) {
	s, repo, _, now, c := captureTest(t)
	c.BroadcastEpoch = 1
	c.SegmentFrames = 300
	received := *now
	c.LastMediaConfirmedAt = &received
	for n := 0; n < 4; n++ {
		for _, q := range []string{"480p", "720p", "1080p"} {
			if n == 3 && q == "1080p" {
				continue
			}
			c.Objects = append(c.Objects, RecordingCaptureObject{RecordingPackageObject: RecordingPackageObject{Path: fmt.Sprintf("%s/seg-%06d.m4s", q, n)}, State: "queued"})
		}
	}
	repo.captures[c.ID] = c
	*now = now.Add(70 * time.Second)
	v, e := s.BroadcastSignal(context.Background(), c.ID, c.ActorID, true)
	if e != nil || v.EndSequenceExclusive != nil || v.State != "interrupted" {
		t.Fatalf("before deadline %+v %v", v, e)
	}
	*now = now.Add(time.Second)
	v, e = s.BroadcastSignal(context.Background(), c.ID, c.ActorID, false)
	if e != nil || v.EndSequenceExclusive != nil || v.State != "interrupted" {
		t.Fatalf("read-only timeout created cutoff %+v %v", v, e)
	}
	v, e = s.BroadcastSignal(context.Background(), c.ID, c.ActorID, true)
	if e != nil || v.EndSequenceExclusive == nil || *v.EndSequenceExclusive != 3 {
		t.Fatalf("cutoff %+v %v", v, e)
	}
	c = repo.captures[c.ID]
	c.Objects = append(c.Objects, RecordingCaptureObject{RecordingPackageObject: RecordingPackageObject{Path: "1080p/seg-000003.m4s"}, State: "queued"})
	fresh := *now
	c.LastMediaConfirmedAt = &fresh
	repo.captures[c.ID] = c
	v, e = s.BroadcastSignal(context.Background(), c.ID, c.ActorID, false)
	if e != nil || *v.EndSequenceExclusive != 3 {
		t.Fatalf("late upload changed cutoff %+v %v", v, e)
	}
	if _, e = s.BroadcastSignal(context.Background(), c.ID, "wrong", true); e != ErrForbidden {
		t.Fatal(e)
	}
}
func TestCaptureSignalNoMediaCannotEndAndFreshMediaRecovers(t *testing.T) {
	s, repo, _, now, c := captureTest(t)
	c.BroadcastEpoch = 1
	repo.captures[c.ID] = c
	*now = now.Add(time.Hour)
	v, e := s.BroadcastSignal(context.Background(), c.ID, c.ActorID, true)
	if e != nil || v.State != "waiting" || v.EndSequenceExclusive != nil {
		t.Fatalf("%+v %v", v, e)
	}
	fresh := *now
	c.LastMediaConfirmedAt = &fresh
	repo.captures[c.ID] = c
	v, e = s.BroadcastSignal(context.Background(), c.ID, c.ActorID, true)
	if e != nil || v.State != "healthy" || v.EndSequenceExclusive != nil {
		t.Fatalf("%+v %v", v, e)
	}
	if v.Deadline == nil || !v.Deadline.Equal(fresh.Add(90030*time.Millisecond)) {
		t.Fatalf("legacy deadline %+v", v.Deadline)
	}
}

func TestDuplicateConfirmationDoesNotRenewSignalClock(t *testing.T) {
	s, repo, objects, now, c := captureTest(t)
	path := "480p/seg-000000.m4s"
	objects.sizes["recordings/packages/"+c.ID+"/staging/"+path] = 10
	c.Objects = []RecordingCaptureObject{{RecordingPackageObject: RecordingPackageObject{Path: path, SizeBytes: 10}, State: "declared"}}
	repo.captures[c.ID] = c
	if _, e := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm-1", []string{path}); e != nil {
		t.Fatal(e)
	}
	first := *repo.captures[c.ID].LastMediaConfirmedAt
	*now = now.Add(time.Minute)
	for _, key := range []string{"confirm-1", "confirm-2"} {
		if _, e := s.Confirm(context.Background(), c.ID, c.ActorID, key, []string{path}); e != nil {
			t.Fatal(e)
		}
	}
	if !repo.captures[c.ID].LastMediaConfirmedAt.Equal(first) {
		t.Fatal("duplicate renewed clock")
	}
}

func TestCaptureSignalToleratesArrivalJitterWithoutExtendingIdleDeadline(t *testing.T) {
	for _, frames := range []int{300, 900} {
		t.Run(fmt.Sprint(frames), func(t *testing.T) {
			s, repo, _, now, c := captureTest(t)
			c.BroadcastEpoch = 1
			c.SegmentFrames = frames
			received := *now
			c.LastMediaConfirmedAt = &received
			repo.captures[c.ID] = c
			expected := received.Add(time.Duration(frames) * 1001 * time.Second / 30000)
			for _, tc := range []struct {
				delay time.Duration
				state string
			}{
				{0, "healthy"}, {5030 * time.Millisecond, "healthy"},
				{10*time.Second - time.Nanosecond, "healthy"}, {10 * time.Second, "interrupted"},
				{59 * time.Second, "interrupted"},
			} {
				*now = expected.Add(tc.delay)
				got, err := s.BroadcastSignal(context.Background(), c.ID, c.ActorID, false)
				if err != nil || got.State != tc.state || got.Deadline == nil || !got.Deadline.Equal(expected.Add(time.Minute)) || got.EndSequenceExclusive != nil {
					t.Fatalf("delay %v: %+v %v", tc.delay, got, err)
				}
			}
		})
	}
}

func TestCaptureSignalObservesProgressAfterRecoveryStops(t *testing.T) {
	s, repo, now, c := recoveryFixture(t)
	c.Recovery.Active = false
	cutoff := c.Recovery.CommonEnd
	c.IdleEndSequenceExclusive = &cutoff
	// Late confirmed segments and an incomplete rendition after the frozen end.
	appendRecoverySegments(&c, cutoff, cutoff+2, "")
	appendRecoverySegments(&c, cutoff+2, cutoff+3, "720p")
	publishedAt := now.Add(-time.Minute)
	c.Progress = RecordingLiveProgress{LastSequence: 0, LastAdvancedAt: &publishedAt}
	repo.captures[c.ID] = c

	signal, err := s.BroadcastSignal(t.Context(), c.ID, c.ActorID, false)
	if err != nil {
		t.Fatal(err)
	}
	if signal.LastConfirmedSequence == nil || *signal.LastConfirmedSequence != cutoff+1 ||
		signal.LastPublishedSequence == nil || *signal.LastPublishedSequence != 0 ||
		signal.LastPublishedAt == nil || !signal.LastPublishedAt.Equal(publishedAt) {
		t.Fatalf("incorrect capture watermarks: %+v", signal)
	}
	if signal.State != "ended" || signal.EndSequenceExclusive == nil || *signal.EndSequenceExclusive != cutoff ||
		signal.CommonConfirmedEndSequenceExclusive == nil || *signal.CommonConfirmedEndSequenceExclusive != cutoff {
		t.Fatalf("observation changed recovery authority: %+v", signal)
	}
}
