package recordingvalidation

import (
	"fmt"
	"math"

	"hhc/asset-api/internal/assets"
)

// Bump when probeFragment or the supported live media checks change.
const liveValidationVersion = "live-fragment-v1"

// This checks trusted snapshot evidence only; it does not verify storage bytes or publish a package.
func validateLiveArchiveEvidence(inv assets.RecordingPackageInventory, master []byte, history []assets.RecordingLiveSegment) error {
	if _, err := assets.ValidateRecordingInventory(inv); err != nil {
		return err
	}
	if len(inv.Renditions) != 3 || len(history) == 0 {
		return assets.ErrInvalidUpload
	}
	objects := make(map[string]assets.RecordingPackageObject, len(inv.Objects))
	for _, object := range inv.Objects {
		objects[object.Path] = object
	}
	codecs := make(map[string]string, 3)
	var reference []segmentProbe
	for _, r := range inv.Renditions {
		matched := false
		for _, live := range assets.LiveRenditions() {
			if r.Name == live.Name && r.Width == live.Width && r.Height == live.Height && r.AudioBitrate == live.AudioBitrate && r.VideoBitrate == live.VideoBitrate {
				matched = true
			}
		}
		if !matched || r.SegmentCount != len(history) {
			return assets.ErrInvalidUpload
		}
		timeline := make([]segmentProbe, len(history))
		for index, segment := range history {
			fragment, ok := segment.Renditions[r.Name]
			receipt := fragment.Validation
			initPath, segmentPath := r.Name+"/init.mp4", fmt.Sprintf("%s/seg-%06d.m4s", r.Name, index)
			if !ok || segment.Sequence != index || len(segment.Renditions) != 3 || receipt == nil || receipt.Version != liveValidationVersion || receipt.Init.Path != initPath || receipt.Segment.Path != segmentPath || receipt.Init != objects[initPath] || receipt.Segment != objects[segmentPath] || fragment.FrameRate != r.FrameRate || math.IsNaN(fragment.Start) || math.IsNaN(fragment.End) || math.IsInf(fragment.Start, 0) || math.IsInf(fragment.End, 0) || fragment.End <= fragment.Start {
				return assets.ErrInvalidUpload
			}
			timeline[index] = segmentProbe{Start: fragment.Start, End: fragment.End, Codecs: fragment.Codecs, FrameRate: fragment.FrameRate, VideoFrames: fragment.VideoFrames}
		}
		if err := validateRenditionTimeline(r, timeline, reference, inv.PresetVersion); err != nil {
			return err
		}
		codecs[r.Name] = timeline[0].Codecs
		if reference == nil {
			reference = timeline
		}
	}
	return assets.ValidateRecordingMasterCodecs(inv, master, codecs)
}
