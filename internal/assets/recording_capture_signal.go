package assets

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Signal describes upload arrival, not verified playback progress.
type RecordingBroadcastSignal struct {
	State                string     `json:"state"`
	LastMediaConfirmedAt *time.Time `json:"lastMediaConfirmedAt"`
	Deadline             *time.Time `json:"deadline"`
	EndSequenceExclusive *int       `json:"endSequenceExclusive"`
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
		if s.now().Before(expected) {
			return nil
		}
		out.State = "interrupted"
		if s.now().Before(deadline) || !allowClose {
			return nil
		}
		confirmed := map[string]bool{}
		for _, o := range c.Objects {
			if (o.State == "queued" || o.State == "verified") && strings.HasSuffix(o.Path, ".m4s") {
				confirmed[o.Path] = true
			}
		}
		end := 0
		for ; end < 4320; end++ {
			if !confirmed[fmt.Sprintf("480p/seg-%06d.m4s", end)] || !confirmed[fmt.Sprintf("720p/seg-%06d.m4s", end)] || !confirmed[fmt.Sprintf("1080p/seg-%06d.m4s", end)] {
				break
			}
		}
		if end > 0 {
			c.IdleEndSequenceExclusive = &end
			out.EndSequenceExclusive = &end
			out.State = "ended"
		}
		return nil
	})
	return out, err
}
