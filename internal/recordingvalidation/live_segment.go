package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/storage/r2"
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
	if !packageIDPattern.MatchString(captureID) || !packageIDPattern.MatchString(attempt) || sequence < 0 || sequence >= assets.RecordingMaxSegments || p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) {
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
				if p.allowLiveGaps {
					continue
				}
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
	// Each approved rendition has one worker; reserve all scratch before reads.
	if err := checkFragmentScratch(dir, int64(len(renditions))*maxScratch); err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(renditions))
	for index, r := range renditions {
		if _, ok := selected[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, sequence)]; !ok {
			continue
		}
		if _, ok := selected[r.Name+"/init.mp4"]; !ok {
			continue
		}
		jobs <- index
	}
	close(jobs)
	available := make([]bool, len(renditions))
	actuals := make([]segmentProbe, len(renditions))
	reusedInits := make([]bool, len(renditions))
	var workers sync.WaitGroup
	var errorMu sync.Mutex
	var root error
	for worker := 0; worker < len(renditions); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if jobCtx.Err() != nil {
					return
				}
				renditionCtx := jobCtx
				cancelRendition := func() {}
				if p.allowLiveGaps {
					renditionCtx, cancelRendition = context.WithTimeout(jobCtx, 15*time.Second)
				}
				actual, reused, err := p.validateLiveRendition(renditionCtx, pack, prefix, dir, attempt, sequence, renditions[index], selected)
				cancelRendition()
				if err != nil {
					errorMu.Lock()
					if root == nil || errors.Is(root, context.Canceled) && !errors.Is(err, context.Canceled) {
						root = err
					}
					errorMu.Unlock()
					if p.allowLiveGaps && jobCtx.Err() == nil {
						slog.Warn("recording_live_rendition_unavailable", "capture_id", captureID, "claim_id", attempt, "sequence", sequence, "rendition", renditions[index].Name)
						continue
					}
					cancel()
					return
				}
				available[index] = true
				actuals[index] = actual
				reusedInits[index] = reused
			}
		}()
	}
	workers.Wait()
	if root != nil && !p.allowLiveGaps {
		return nil, root
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result = make(map[string]assets.RecordingLiveFragment, len(renditions))
	var frameRate float64
	for index, r := range renditions {
		if !available[index] {
			continue
		}
		actual := actuals[index]
		if actual.AudioChannels != 2 || frameRate != 0 && actual.FrameRate != frameRate {
			return nil, assets.ErrInvalidUpload
		}
		frameRate = actual.FrameRate
		result[r.Name] = assets.RecordingLiveFragment{VideoFrames: actual.VideoFrames, Start: actual.Start, End: actual.End, Codecs: actual.Codecs, FrameRate: actual.FrameRate}
		fragment := result[r.Name]
		fragment.Validation = &assets.RecordingLiveValidation{Version: liveValidationVersion, Init: selected[r.Name+"/init.mp4"], Segment: selected[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, sequence)]}
		if prior, ok := p.reusableLiveInit(r.Name, fragment.Validation.Init); ok && reusedInits[index] {
			fragment.Validation.InitETag = prior.InitETag
		}
		result[r.Name] = fragment
	}
	if len(result) == 0 {
		if root == nil {
			return nil, assets.ErrCaptureMissingObjects
		}
		return nil, root
	}
	return result, nil
}

func (p PackageMediaProbe) validateLiveRendition(ctx context.Context, pack assets.RecordingPackage, prefix, dir, attempt string, sequence int, r assets.RecordingRendition, selected map[string]assets.RecordingPackageObject) (actual segmentProbe, reused bool, err error) {
	started := time.Now()
	var freezeTime, probeTime time.Duration
	defer func() {
		slog.Info("recording_live_rendition", "capture_id", pack.ID, "claim_id", attempt, "sequence", sequence, "rendition", r.Name,
			"freeze_ms", freezeTime.Milliseconds(), "probe_decode_ms", probeTime.Milliseconds(),
			"elapsed_ms", time.Since(started).Milliseconds(), "validated", err == nil,
			"video_frames", actual.VideoFrames, "start_seconds", actual.Start, "end_seconds", actual.End)
	}()
	init := selected[r.Name+"/init.mp4"]
	segment := selected[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, sequence)]
	if err := checkFragmentScratch(dir, init.SizeBytes+segment.SizeBytes); err != nil {
		return actual, reused, err
	}
	path := filepath.Join(dir, r.Name+".mp4")
	freezeStarted := time.Now()
	// Bound provider I/O independently of the decoder and the renewable claim.
	freezeCtx, cancelFreeze := context.WithTimeout(ctx, time.Minute)
	observation := &liveFreeze{PackageObjects: p.Objects, part: "init"}
	p.Objects = observation
	freezeCtx = r2.WithLiveAttempt(freezeCtx, pack.ID, attempt, r.Name, "freeze", sequence)
	freezeCtx = r2.WithAttemptCounter(freezeCtx, &observation.attempts)
	err = writePackageScratchObserved(path, observation.record, func(w io.Writer) error {
		w = freezeWriter{w, observation}
		var initErr error
		if _, ok := p.reusableLiveInit(r.Name, init); ok {
			reused, initErr = p.copyLiveInit(freezeCtx, pack.ID, init, w)
		}
		if initErr == nil && !reused {
			observation.failed = "" // A missing reusable init falls back to staging.
			initErr = freezePackageObject(freezeCtx, pack, prefix, init, p.Objects, w, p.OnStage)
		}
		if err := initErr; err != nil {
			return err
		}
		observation.part = "segment"
		return freezePackageObject(freezeCtx, pack, prefix, segment, p.Objects, w, p.OnStage)
	})

	if freezeCtx.Err() != nil {
		err = errors.Join(err, freezeCtx.Err())
	}
	cancelFreeze()
	freezeTime = time.Since(freezeStarted)
	observation.report(pack.ID, attempt, r.Name, sequence, freezeTime, err, ctx.Err())
	if err != nil {
		return actual, reused, err
	}
	r.FrameRate = 0 // Infer the approved rate from this immutable fragment.
	probeStarted := time.Now()
	actual, err = p.probeFragment(ctx, path, r)
	probeTime = time.Since(probeStarted)
	if err != nil {
		return actual, reused, err
	}
	if err := os.Remove(path); err != nil {
		return actual, reused, err
	}
	return actual, reused, nil
}

func (p PackageMediaProbe) reusableLiveInit(name string, init assets.RecordingPackageObject) (assets.RecordingLiveValidation, bool) {
	prior, ok := p.LiveInitEvidence[name]
	return prior, ok && prior.Version == liveValidationVersion && prior.Init == init && prior.InitETag != ""
}

// Stable init reuse still hashes the exact bytes appended to every decoded fragment.
func (p PackageMediaProbe) copyLiveInit(ctx context.Context, capture string, object assets.RecordingPackageObject, destination io.Writer) (bool, error) {
	body, err := p.Objects.Open(ctx, "recordings/captures/"+capture+"/final/"+object.Path)
	if errors.Is(err, r2.ErrNotFound) || errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	hash := sha256.New()
	size, readErr := io.Copy(io.MultiWriter(hash, destination), io.LimitReader(body, object.SizeBytes+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return true, err
	}
	if size != object.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != object.SHA256 {
		return true, assets.ErrInvalidUpload
	}
	return true, nil
}
