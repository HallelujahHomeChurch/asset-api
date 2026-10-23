package assets

import (
	"context"
	"time"
)

// Signal describes upload arrival, not verified playback progress.
type RecordingBroadcastSignal struct {
	RecoveryVersion                     string     `json:"recoveryVersion,omitempty"`
	LastCommonConfirmedAt               *time.Time `json:"lastCommonConfirmedAt,omitempty"`
	CommonConfirmedEndSequenceExclusive *int       `json:"commonConfirmedEndSequenceExclusive,omitempty"`
	Reason                              *string    `json:"reason,omitempty"`
	State                               string     `json:"state"`
	LastMediaConfirmedAt                *time.Time `json:"lastMediaConfirmedAt"`
	Deadline                            *time.Time `json:"deadline"`
	EndSequenceExclusive                *int       `json:"endSequenceExclusive"`
}

// The capture transaction serializes timeout with new confirms. A frozen cutoff
// never rejects later supplementation or pretends those bytes passed validation.
func (s *RecordingCaptureService) BroadcastSignal(ctx context.Context, id, actor string, allowClose bool) (RecordingBroadcastSignal, error) {
	var out RecordingBroadcastSignal
	_, err := s.repository.UpdateCapture(ctx, id, func(c *RecordingCapture) error {
		if err := captureOwner(*c, actor); err != nil {
			return err
		}
		if c.BroadcastEpoch < 1 {
			return ErrConflict
		}
		if c.Recovery.Version == RecordingRecoveryVersion {
			out.RecoveryVersion = RecordingRecoveryVersion
			out.State = "waiting"
			out.LastMediaConfirmedAt = c.LastMediaConfirmedAt
			out.LastCommonConfirmedAt = c.Recovery.LastCommonConfirmedAt
			out.Deadline = c.Recovery.Deadline
			if c.Recovery.ActivationAt != nil {
				end := c.Recovery.CommonEnd
				out.CommonConfirmedEndSequenceExclusive = &end
			}
			if c.Recovery.Active && c.State == "uploading" && c.TerminalAt == nil && c.Recovery.Deadline != nil {
				out.State = "healthy"
				if !s.now().Before(c.Recovery.Deadline.Add(-recordingRecoveryWindow + 30*time.Second)) {
					out.State = "interrupted"
				}
				if allowClose {
					closeExpiredCaptureRecovery(c, s.now())
				}
			}
			out.EndSequenceExclusive = c.IdleEndSequenceExclusive
			if c.IdleEndSequenceExclusive != nil {
				out.State = "ended"
				reason := "idle_timeout"
				out.Reason = &reason
			}
			return nil
		}
		out.State = "waiting"
		out.LastMediaConfirmedAt = c.LastMediaConfirmedAt
		out.EndSequenceExclusive = c.IdleEndSequenceExclusive
		if c.IdleEndSequenceExclusive != nil {
			out.State = "ended"
			return nil
		}
		if c.LastMediaConfirmedAt == nil || c.State != "uploading" || c.TerminalAt != nil {
			return nil
		}
		frames := c.SegmentFrames
		if frames == 0 {
			frames = 900
		}
		expected := c.LastMediaConfirmedAt.Add(time.Duration(frames) * 1001 * time.Second / 30000)
		deadline := expected.Add(time.Minute)
		out.Deadline = &deadline
		out.State = "healthy"
		// Arrival includes encoding/upload jitter; this tolerance does not move the idle cutoff.
		if s.now().Before(expected.Add(10 * time.Second)) {
			return nil
		}
		out.State = "interrupted"
		if s.now().Before(deadline) || !allowClose {
			return nil
		}
		end := confirmedCaptureEnd(*c)
		if end > 0 {
			c.IdleEndSequenceExclusive = &end
			out.EndSequenceExclusive = &end
			out.State = "ended"
		}
		return nil
	})
	return out, err
}
