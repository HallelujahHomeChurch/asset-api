package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"log/slog"
	"strings"
	"time"
)

type LivePreviewObjects interface {
	PreviewObjects
	AdvanceLivePreviewPointer(context.Context, string, int) error
}

// Shared segment images survive promotion; only a cumulative index is finalized.
func (p PackageMediaProbe) generateSharedPreviews(ctx context.Context, id, mediaPrefix string, inventory []assets.RecordingPackageObject, r assets.RecordingRendition, timeline []assets.RecordingLiveSegment, first int, previous string, objects LivePreviewObjects) (string, error) {
	if !liveCoverCaptureID(id) || len(timeline) != r.SegmentCount || first < 0 || first > r.SegmentCount {
		return "", assets.ErrInvalidInput
	}
	prefix := "recordings/captures/" + id + "/final/previews/"
	var vtt string
	err := p.generatePreviewRange(ctx, mediaPrefix, prefix+"shared/", inventory, r, timeline, first, previous, func(ctx context.Context, key string, data []byte) error {
		if strings.HasSuffix(key, "/index.vtt") {
			vtt = string(data)
			key = fmt.Sprintf("%s%d/index.vtt", prefix, r.SegmentCount)
		}
		return objects.PutPackagePreview(ctx, key, data)
	})
	if err != nil {
		return "", err
	}
	return vtt, nil
}

func RunLivePreview(ctx context.Context, repo *postgres.RecordingPackageStore, objects LivePreviewObjects, probe PackageMediaProbe) (bool, error) {
	c, err := repo.ClaimLivePreview(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	started := time.Now()
	slog.Info("recording_live_preview_claim", "capture_id", c.CaptureID, "claim_id", c.ClaimID, "first_sequence", c.Completed+1, "last_sequence", c.Last)
	work, cancel := context.WithTimeout(ctx, 20*time.Second)
	r := assets.RecordingRendition{Name: "480p", SegmentCount: c.Last + 1}
	vtt, err := probe.generateSharedPreviews(work, c.CaptureID, "recordings/captures/"+c.CaptureID+"/final/", c.Objects, r, c.Timeline, c.Completed+1, c.VTT, objects)
	cancel()
	finish, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()
	var publish func(context.Context) error
	if err == nil {
		publish = func(ctx context.Context) error { return objects.AdvanceLivePreviewPointer(ctx, c.CaptureID, c.Last) }
	}
	finishErr := repo.FinishLivePreview(finish, c, vtt, publish)
	slog.Info("recording_live_preview_result", "capture_id", c.CaptureID, "claim_id", c.ClaimID, "last_sequence", c.Last, "elapsed_ms", time.Since(started).Milliseconds(), "generated", err == nil, "committed", finishErr == nil)
	return true, errors.Join(err, finishErr)
}
