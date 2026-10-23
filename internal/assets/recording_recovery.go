package assets

import (
	"fmt"
	"time"
)

const RecordingRecoveryVersion = "common-watermark-v1"
const recordingRecoveryWindow = 5 * time.Minute

// Recovery is persisted authority. It describes bounded upload arrival, not
// proof of a currently running encoder or verification of every arrived byte.
type RecordingCaptureRecovery struct {
	Version               string
	Active                bool
	ActivationAt          *time.Time
	BaselineEnd           int
	SegmentDuration       time.Duration
	CommonEnd             int
	LastCommonConfirmedAt *time.Time
	Deadline              *time.Time
}

func confirmedCaptureEnd(c RecordingCapture) int {
	confirmed := make(map[string]bool, len(c.Objects))
	for _, o := range c.Objects {
		if o.State == "queued" || o.State == "verified" {
			confirmed[o.Path] = true
		}
	}
	end := 0
	for ; end < 4320; end++ {
		if !confirmed[fmt.Sprintf("480p/seg-%06d.m4s", end)] || !confirmed[fmt.Sprintf("720p/seg-%06d.m4s", end)] || !confirmed[fmt.Sprintf("1080p/seg-%06d.m4s", end)] {
			break
		}
	}
	return end
}

// ArmCaptureRecovery is called in the successful live range transaction. The
// verified rate comes from decoded common history, not a client clock.
func ArmCaptureRecovery(c *RecordingCapture, policy RecordingBroadcastRange, verifiedRate float64, now time.Time) error {
	r := &c.Recovery
	if r.Version != RecordingRecoveryVersion {
		return nil
	}
	if policy.MemberState != "live" || policy.Revoked || policy.EndSequenceExclusive != nil || c.TerminalAt != nil || c.State != "uploading" {
		r.Active = false
		return nil
	}
	if policy.MemberState != "live" || policy.StartSequence == nil || c.IdleEndSequenceExclusive != nil || r.ActivationAt != nil {
		return nil
	}
	var duration time.Duration
	switch {
	case c.SegmentFrames == 300 && verifiedRate == 30000.0/1001:
		duration = 10010 * time.Millisecond
	case (c.SegmentFrames == 0 || c.SegmentFrames == 900) && verifiedRate == 30:
		duration = 30 * time.Second
	case (c.SegmentFrames == 0 || c.SegmentFrames == 900) && verifiedRate == 30000.0/1001:
		duration = 30030 * time.Millisecond
	default:
		return ErrConflict
	}
	end := confirmedCaptureEnd(*c)
	if end <= *policy.StartSequence {
		return ErrCaptureMissingObjects
	}
	now = now.UTC().Truncate(time.Microsecond)
	deadline := now.Add(recordingRecoveryWindow)
	*r = RecordingCaptureRecovery{Version: RecordingRecoveryVersion, Active: true, ActivationAt: &now, BaselineEnd: end, SegmentDuration: duration, CommonEnd: end, LastCommonConfirmedAt: &now, Deadline: &deadline}
	return nil
}

// Expiry is checked against the old deadline before new bytes can renew it.
func closeExpiredCaptureRecovery(c *RecordingCapture, now time.Time) {
	r := &c.Recovery
	if r.Active && r.Deadline != nil && !now.Before(*r.Deadline) && c.IdleEndSequenceExclusive == nil && r.CommonEnd > 0 {
		end := r.CommonEnd
		c.IdleEndSequenceExclusive = &end
		r.Active = false
	}
}
func advanceCaptureRecovery(c *RecordingCapture, now time.Time) {
	r := &c.Recovery
	if !r.Active || r.ActivationAt == nil || r.Deadline == nil || c.IdleEndSequenceExclusive != nil {
		return
	}
	// A backwards local clock must not move persisted authority backwards.
	if r.LastCommonConfirmedAt != nil && now.Before(*r.LastCommonConfirmedAt) {
		return
	}
	end := confirmedCaptureEnd(*c)
	if end <= r.CommonEnd {
		return
	}
	now = now.UTC().Truncate(time.Microsecond)
	horizon := r.ActivationAt.Add(time.Duration(end-r.BaselineEnd) * r.SegmentDuration)
	freshness := now
	if horizon.Before(freshness) {
		freshness = horizon
	}
	deadline := freshness.Add(recordingRecoveryWindow)
	r.CommonEnd = end
	r.LastCommonConfirmedAt = &now
	r.Deadline = &deadline
}
