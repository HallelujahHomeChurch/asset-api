package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ValidateLiveSegment decodes only the next three closed fragments. Copies are
// attempt-private; the caller publishes them only after the whole batch passes.
func (p PackageMediaProbe) ValidateLiveSegment(ctx context.Context, captureID, attempt string, sequence int, declarations []assets.RecordingPackageObject) (result map[string]assets.RecordingLiveFragment, err error) {
	if !packageIDPattern.MatchString(captureID) || !packageIDPattern.MatchString(attempt) || sequence < 0 || sequence >= 1440 || p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) {
		return nil, assets.ErrInvalidInput
	}
	selected := map[string]assets.RecordingPackageObject{}
	for _, o := range declarations {
		if _, exists := selected[o.Path]; exists {
			return nil, assets.ErrInvalidInput
		}
		selected[o.Path] = o
	}
	for _, r := range assets.LiveRenditions() {
		for _, name := range []string{"init.mp4", fmt.Sprintf("seg-%06d.m4s", sequence)} {
			o, ok := selected[r.Name+"/"+name]
			if !ok {
				return nil, assets.ErrCaptureMissingObjects
			}
			if o.SizeBytes < 1 || o.SizeBytes > assets.RecordingObjectMaxBytes || len(o.SHA256) != 64 {
				return nil, assets.ErrInvalidInput
			}
		}
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-live-fragment-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	pack := assets.RecordingPackage{ID: captureID}
	prefix := "recordings/packages/" + captureID + "/final/" + attempt + "/"
	renditions := assets.LiveRenditions()
	var maxScratch int64
	for _, r := range renditions {
		maxScratch = max(maxScratch, selected[r.Name+"/init.mp4"].SizeBytes+selected[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, sequence)].SizeBytes)
	}
	// Reserve both workers before starting any provider reads.
	if err := checkFragmentScratch(dir, 2*maxScratch); err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(renditions))
	for index := range renditions {
		jobs <- index
	}
	close(jobs)
	actuals := make([]segmentProbe, len(renditions))
	var workers sync.WaitGroup
	var errorMu sync.Mutex
	var root error
	for worker := 0; worker < 2; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if jobCtx.Err() != nil {
					return
				}
				actual, err := p.validateLiveRendition(jobCtx, pack, prefix, dir, attempt, sequence, renditions[index], selected)
				if err != nil {
					errorMu.Lock()
					if root == nil || errors.Is(root, context.Canceled) && !errors.Is(err, context.Canceled) {
						root = err
					}
					errorMu.Unlock()
					cancel()
					return
				}
				actuals[index] = actual
			}
		}()
	}
	workers.Wait()
	if root != nil {
		return nil, root
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result = make(map[string]assets.RecordingLiveFragment, len(renditions))
	for index, r := range renditions {
		actual := actuals[index]
		if actual.AudioChannels != 2 || actual.FrameRate != actuals[0].FrameRate {
			return nil, assets.ErrInvalidUpload
		}
		result[r.Name] = assets.RecordingLiveFragment{Start: actual.Start, End: actual.End, Codecs: actual.Codecs, FrameRate: actual.FrameRate}
	}
	return result, nil
}

func (p PackageMediaProbe) validateLiveRendition(ctx context.Context, pack assets.RecordingPackage, prefix, dir, attempt string, sequence int, r assets.RecordingRendition, selected map[string]assets.RecordingPackageObject) (actual segmentProbe, err error) {
	started := time.Now()
	var freezeTime, probeTime time.Duration
	defer func() {
		slog.Info("recording_live_rendition", "capture_id", pack.ID, "claim_id", attempt, "sequence", sequence, "rendition", r.Name,
			"freeze_ms", freezeTime.Milliseconds(), "probe_decode_ms", probeTime.Milliseconds(),
			"elapsed_ms", time.Since(started).Milliseconds(), "validated", err == nil)
	}()
	init := selected[r.Name+"/init.mp4"]
	segment := selected[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, sequence)]
	if err := checkFragmentScratch(dir, init.SizeBytes+segment.SizeBytes); err != nil {
		return actual, err
	}
	path := filepath.Join(dir, r.Name+".mp4")
	freezeStarted := time.Now()
	err = writePackageScratch(path, func(w io.Writer) error {
		if err := freezePackageObject(ctx, pack, prefix, init, p.Objects, w); err != nil {
			return err
		}
		return freezePackageObject(ctx, pack, prefix, segment, p.Objects, w)
	})
	freezeTime = time.Since(freezeStarted)
	if err != nil {
		return actual, err
	}
	r.FrameRate = 0 // Infer the approved rate from this immutable fragment.
	probeStarted := time.Now()
	actual, err = p.probeFragment(ctx, path, r)
	probeTime = time.Since(probeStarted)
	if err != nil {
		return actual, err
	}
	if err := os.Remove(path); err != nil {
		return actual, err
	}
	return actual, nil
}
