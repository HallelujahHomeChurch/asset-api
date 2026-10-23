package assets

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func appendRecoverySegments(c *RecordingCapture, start, end int, quality string) {
	for n := start; n < end; n++ {
		for _, q := range []string{"480p", "720p", "1080p"} {
			if quality != "" && q != quality {
				continue
			}
			c.Objects = append(c.Objects, RecordingCaptureObject{RecordingPackageObject: RecordingPackageObject{Path: fmt.Sprintf("%s/seg-%06d.m4s", q, n)}, State: "queued"})
		}
	}
}
func recoveryFixture(t *testing.T) (*RecordingCaptureService, *captureRepo, *time.Time, RecordingCapture) {
	t.Helper()
	s, repo, _, now, c := captureTest(t)
	c.BroadcastEpoch = 1
	c.SegmentFrames = 300
	c.Recovery.Version = RecordingRecoveryVersion
	appendRecoverySegments(&c, 0, 10, "")
	start := 0
	if err := ArmCaptureRecovery(&c, RecordingBroadcastRange{MemberState: "live", StartSequence: &start}, 30000.0/1001, *now); err != nil {
		t.Fatal(err)
	}
	repo.captures[c.ID] = c
	return s, repo, now, c
}
func TestRecoveryFiveMinuteBoundaryAndRestart(t *testing.T) {
	for _, delay := range []time.Duration{30*time.Second - time.Microsecond, 30 * time.Second, 299 * time.Second, 300 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			s, repo, now, c := recoveryFixture(t)
			*now = now.Add(delay)
			// Reconstruct a service from the same persisted state, with no local timer.
			s = NewRecordingCaptureService(repo, s.objects, func() time.Time { return *now })
			signal, err := s.BroadcastSignal(context.Background(), c.ID, c.ActorID, true)
			if err != nil {
				t.Fatal(err)
			}
			want := "healthy"
			if delay >= 30*time.Second {
				want = "interrupted"
			}
			if delay >= 300*time.Second {
				want = "ended"
			}
			if signal.State != want || signal.Deadline == nil || !signal.Deadline.Equal(*c.Recovery.Deadline) {
				t.Fatalf("signal=%+v want=%s", signal, want)
			}
			if delay >= 300*time.Second && (signal.EndSequenceExclusive == nil || *signal.EndSequenceExclusive != 10 || signal.Reason == nil || *signal.Reason != "idle_timeout") {
				t.Fatal("cutoff missing")
			}
		})
	}
}
func TestRecoveryCommonWatermarkAndBoundedBacklog(t *testing.T) {
	_, _, now, c := recoveryFixture(t)
	original := *c.Recovery.Deadline
	appendRecoverySegments(&c, 10, 11, "480p")
	advanceCaptureRecovery(&c, now.Add(299*time.Second))
	if !c.Recovery.Deadline.Equal(original) {
		t.Fatal("single rendition renewed deadline")
	}
	appendRecoverySegments(&c, 10, 11, "720p")
	appendRecoverySegments(&c, 10, 11, "1080p")
	advanceCaptureRecovery(&c, now.Add(299*time.Second))
	if !c.Recovery.Deadline.Equal(original.Add(10010 * time.Millisecond)) {
		t.Fatal("stale segment received fresh five-minute window")
	}
	accepted := *c.Recovery.LastCommonConfirmedAt
	advanceCaptureRecovery(&c, now.Add(300*time.Second))
	if !c.Recovery.LastCommonConfirmedAt.Equal(accepted) {
		t.Fatal("duplicate renewed common acceptance")
	}
	appendRecoverySegments(&c, 12, 13, "")
	advanceCaptureRecovery(&c, now.Add(300*time.Second))
	if c.Recovery.CommonEnd != 11 {
		t.Fatal("gap advanced common watermark")
	}
}
func TestRecoveryCatchupBeforeDeadlineAndLateConfirmation(t *testing.T) {
	_, _, now, c := recoveryFixture(t)
	appendRecoverySegments(&c, 10, 22, "")
	closeExpiredCaptureRecovery(&c, now.Add(120*time.Second))
	advanceCaptureRecovery(&c, now.Add(120*time.Second))
	if !c.Recovery.Deadline.Equal(now.Add(420 * time.Second)) {
		t.Fatal("timely catchup did not recover")
	}
	closeExpiredCaptureRecovery(&c, now.Add(420*time.Second))
	appendRecoverySegments(&c, 22, 30, "")
	advanceCaptureRecovery(&c, now.Add(420*time.Second))
	if c.IdleEndSequenceExclusive == nil || *c.IdleEndSequenceExclusive != 22 || c.Recovery.CommonEnd != 22 {
		t.Fatal("late bytes reopened cutoff")
	}
}
func TestRecoveryActivationDoesNotResetOrRevive(t *testing.T) {
	_, _, now, c := recoveryFixture(t)
	original := c.Recovery
	start := 0
	live := RecordingBroadcastRange{MemberState: "live", StartSequence: &start}
	if err := ArmCaptureRecovery(&c, live, 30, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c.Recovery != original {
		t.Fatal("repeat range reset authority")
	}
	live.Revoked = true
	if err := ArmCaptureRecovery(&c, live, 30, *now); err != nil {
		t.Fatal(err)
	}
	live.Revoked = false
	if err := ArmCaptureRecovery(&c, live, 30, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c.Recovery.Active || !c.Recovery.Deadline.Equal(*original.Deadline) {
		t.Fatal("revoked recovery rearmed")
	}
	legacy := c
	legacy.Recovery = RecordingCaptureRecovery{}
	if err := ArmCaptureRecovery(&legacy, live, 30, *now); err != nil || legacy.Recovery.Version != "" {
		t.Fatal("legacy capture upgraded")
	}
}
func TestRecoveryProfileDuration(t *testing.T) {
	for _, tc := range []struct {
		frames   int
		rate     float64
		duration time.Duration
	}{{300, 30000.0 / 1001, 10010 * time.Millisecond}, {900, 30, 30 * time.Second}, {900, 30000.0 / 1001, 30030 * time.Millisecond}} {
		_, _, now, c := recoveryFixture(t)
		c.SegmentFrames = tc.frames
		c.Recovery = RecordingCaptureRecovery{Version: RecordingRecoveryVersion}
		start := 0
		if err := ArmCaptureRecovery(&c, RecordingBroadcastRange{MemberState: "live", StartSequence: &start}, tc.rate, *now); err != nil || c.Recovery.SegmentDuration != tc.duration {
			t.Fatalf("profile %+v: %v", tc, err)
		}
	}
}

func TestRecoveryCreationOptInIsImmutable(t *testing.T) {
	for _, version := range []string{"", RecordingRecoveryVersion} {
		t.Run(version, func(t *testing.T) {
			s, repo, _, _, _ := captureTest(t)
			record := "33333333-3333-4333-8333-333333333333"
			actor := "22222222-2222-4222-8222-222222222222"
			first, err := s.CreateBroadcastWithRecovery(context.Background(), record, actor, "broadcast", 1, 300, version)
			if err != nil || repo.captures[first.Capture.ID].Recovery.Version != version {
				t.Fatalf("create: %v", err)
			}
			if _, err = s.CreateBroadcastWithRecovery(context.Background(), record, actor, "broadcast", 1, 300, version); err != nil {
				t.Fatal(err)
			}
			changed := RecordingRecoveryVersion
			if version != "" {
				changed = ""
			}
			if _, err = s.CreateBroadcastWithRecovery(context.Background(), record, actor, "broadcast", 1, 300, changed); err != ErrConflict {
				t.Fatalf("replay upgraded/downgraded: %v", err)
			}
			if _, err = s.CreateBroadcastWithRecovery(context.Background(), record, actor, "unknown", 2, 300, "future"); err != ErrInvalidInput {
				t.Fatalf("unknown capability: %v", err)
			}
		})
	}
}

func TestRecoveryNeverArmsWithoutVerifiedProfileOrAfterCutoff(t *testing.T) {
	_, _, now, c := recoveryFixture(t)
	c.Recovery = RecordingCaptureRecovery{Version: RecordingRecoveryVersion}
	start := 0
	policy := RecordingBroadcastRange{MemberState: "live", StartSequence: &start}
	if err := ArmCaptureRecovery(&c, policy, 30, *now); err != ErrConflict || c.Recovery.Active {
		t.Fatalf("wrong rate armed: %v", err)
	}
	cutoff := 5
	c.IdleEndSequenceExclusive = &cutoff
	if err := ArmCaptureRecovery(&c, policy, 30000.0/1001, *now); err != nil || c.Recovery.Active {
		t.Fatal("old cutoff rearmed")
	}
}
