package recordingvalidation

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

// Only a missing rendition is fetched and decoded. Its initialization bytes and
// timeline must match the already published broadcast before it can be repaired.
func claimLiveRepair(ctx context.Context, store *postgres.RecordingCaptureStore, objects LiveObjects, probe PackageMediaProbe) (func() error, error) {
	admit, cancel := context.WithTimeout(ctx, liveAdmissionTimeout)
	defer cancel()
	repair, err := store.ClaimLiveRepair(admit)
	if errors.Is(err, assets.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		work, cancel := context.WithTimeout(ctx, 20*time.Second)
		fragment, workErr := repairLiveRendition(work, objects, probe, repair)
		cancel()
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.FinishLiveRepair(finish, repair, fragment); err != nil {
			return err
		}
		slog.Info("recording_live_repair_attempt", "capture_id", repair.Capture.ID, "sequence", repair.Sequence, "rendition", repair.Rendition, "validated", workErr == nil)
		return nil
	}, nil
}

func repairLiveRendition(ctx context.Context, objects LiveObjects, probe PackageMediaProbe, r postgres.LiveRepair) (*assets.RecordingLiveFragment, error) {
	dir, err := os.MkdirTemp(probe.ScratchRoot, "hhc-live-repair-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	init, seg := r.Objects[0], r.Objects[1]
	established := r.Snapshot.Segments[0].Renditions[r.Rendition].Validation
	if established == nil || established.Init != init || established.InitETag == "" {
		return nil, assets.ErrInvalidUpload
	}
	probe.LiveInitEvidence = map[string]assets.RecordingLiveValidation{r.Rendition: *established}
	prefix := "recordings/packages/" + r.Capture.ID + "/final/" + r.ClaimID + "/"
	for _, profile := range assets.LiveRenditions() {
		if profile.Name != r.Rendition {
			continue
		}
		actual, reused, err := probe.validateLiveRendition(ctx, assets.RecordingPackage{ID: r.Capture.ID}, prefix, dir, r.ClaimID, r.Sequence, profile, map[string]assets.RecordingPackageObject{init.Path: init, seg.Path: seg})
		if err != nil {
			return nil, err
		}
		gap := r.Snapshot.Segments[r.Sequence].Renditions[r.Rendition]
		tolerance := 1/gap.FrameRate + 0.001
		if !reused || !gap.Gap || actual.AudioChannels != 2 || actual.FrameRate != gap.FrameRate || actual.Codecs != gap.Codecs || actual.VideoFrames != gap.VideoFrames || math.Abs(actual.Start-gap.Start) > tolerance || math.Abs(actual.End-gap.End) > tolerance {
			return nil, assets.ErrInvalidUpload
		}
		gap.Gap = false
		gap.Start, gap.End = actual.Start, actual.End
		prev := r.Snapshot.Segments[r.Sequence-1].Renditions[r.Rendition]
		if math.Abs(prev.End-actual.Start) > tolerance {
			return nil, assets.ErrInvalidUpload
		}
		if r.Sequence+1 < len(r.Snapshot.Segments) && math.Abs(r.Snapshot.Segments[r.Sequence+1].Renditions[r.Rendition].Start-actual.End) > tolerance {
			return nil, assets.ErrInvalidUpload
		}
		gap.Validation = &assets.RecordingLiveValidation{Version: liveValidationVersion, Init: init, InitETag: established.InitETag, Segment: seg}
		versions, err := copyLiveObjects(ctx, objects, r.RecordingLiveClaim, map[string]assets.RecordingLiveFragment{r.Rendition: gap}, &liveCopyTiming{})
		if err != nil {
			return nil, err
		}
		gap.Validation.SegmentETag = versions[seg.Path]
		return &gap, nil
	}
	return nil, assets.ErrInvalidInput
}
