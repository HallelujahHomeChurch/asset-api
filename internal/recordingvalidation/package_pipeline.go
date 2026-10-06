package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
)

// Both immutable freezing and final-only validation use the same media checks.
// One rendition at a time owns its init cache and a single two-worker budget.
func (p PackageMediaProbe) validateMedia(ctx context.Context, inv assets.RecordingPackageInventory, master []byte, init func(context.Context, string, io.Writer) error, fragment func(context.Context, string, io.Writer) error) (result error) {
	if !filepath.IsAbs(p.FFmpeg) || !filepath.IsAbs(p.FFprobe) {
		return assets.ErrInvalidInput
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-fragment-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	sizes := make(map[string]int64, len(inv.Objects))
	var maxInit, maxSegment int64
	for _, o := range inv.Objects {
		sizes[o.Path] = o.SizeBytes
		if filepath.Base(o.Path) == "init.mp4" && o.SizeBytes > maxInit {
			maxInit = o.SizeBytes
		}
		if filepath.Ext(o.Path) == ".m4s" && o.SizeBytes > maxSegment {
			maxSegment = o.SizeBytes
		}
	}
	// Reserve the cached init plus two init+fragment scratch files, with a
	// separate free-space margin. The entire package is never materialized.
	reservation := maxInit + 2*(maxInit+maxSegment)
	if maxInit <= 0 || maxSegment <= 0 || maxInit > assets.RecordingObjectMaxBytes || maxSegment > assets.RecordingObjectMaxBytes || reservation > 1<<30 {
		return assets.ErrInvalidUpload
	}
	if err := checkFragmentScratch(dir, reservation); err != nil {
		return err
	}
	var reference []segmentProbe
	started := time.Now().UTC()
	var objectDone, segmentDone, bytesDone, segmentTotal, bytesTotal int64
	for _, o := range inv.Objects {
		bytesTotal += o.SizeBytes
		if filepath.Ext(o.Path) == ".m3u8" {
			objectDone++
			bytesDone += o.SizeBytes
		}
		if filepath.Ext(o.Path) == ".m4s" {
			segmentTotal++
		}
	}
	emit := func(phase, rendition string) {
		if p.OnProgress == nil {
			return
		}
		now := time.Now().UTC()
		objects, segments, size, total := objectDone, segmentDone, bytesDone, int64(len(inv.Objects))
		phaseStart := started
		if phase == "package_finalization" {
			phaseStart = now
		}
		p.OnProgress(assets.RecordingProcessingProgress{Attempt: 1, Phase: phase, Rendition: rendition, ObjectsVerified: &objects, ObjectsTotal: &total, SegmentsVerified: &segments, SegmentsTotal: &segmentTotal, BytesVerified: &size, BytesTotal: &bytesTotal, AttemptStartedAt: started, PhaseStartedAt: phaseStart, LastProgressAt: now, HeartbeatAt: now})
	}
	codecs := make(map[string]string, len(inv.Renditions))
	for _, r := range inv.Renditions {
		if r.SegmentCount <= 0 || r.SegmentCount > assets.RecordingPackageMaxObjects || math.IsNaN(r.FrameRate) || math.IsInf(r.FrameRate, 0) || r.FrameRate <= 0 {
			return assets.ErrInvalidUpload
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		initPath := filepath.Join(dir, "init.mp4")
		if err := writePackageScratch(initPath, func(w io.Writer) error { return init(ctx, r.Name+"/init.mp4", w) }); err != nil {
			return err
		}
		objectDone++
		bytesDone += sizes[r.Name+"/init.mp4"]
		emit("package_validation", r.Name)
		jobCtx, cancel := context.WithCancel(ctx)
		timeline := make([]segmentProbe, r.SegmentCount)
		jobs := make(chan int, r.SegmentCount)
		for index := 0; index < r.SegmentCount; index++ {
			jobs <- index
		}
		close(jobs)
		type fragmentResult struct {
			index int
			probe segmentProbe
		}
		results := make(chan fragmentResult, 2)
		var workers sync.WaitGroup
		var errorMu sync.Mutex
		var root error
		fail := func(err error) {
			errorMu.Lock()
			if root == nil || (errors.Is(root, context.Canceled) && !errors.Is(err, context.Canceled)) {
				root = err
			}
			errorMu.Unlock()
			cancel()
		}
		for worker := 0; worker < 2; worker++ {
			workers.Add(1)
			go func(worker int) {
				defer workers.Done()
				for index := range jobs {
					if err := jobCtx.Err(); err != nil {
						return
					}
					name := fmt.Sprintf("%s/seg-%06d.m4s", r.Name, index)
					path := filepath.Join(dir, fmt.Sprintf("fragment-%d.mp4", worker))
					var actual segmentProbe
					err := func() error {
						if err := checkFragmentScratch(dir, sizes[r.Name+"/init.mp4"]+sizes[name]); err != nil {
							return err
						}
						if err := writePackageScratch(path, func(w io.Writer) error {
							cached, err := os.Open(initPath)
							if err != nil {
								return err
							}
							_, readErr := io.Copy(w, io.LimitReader(cached, sizes[r.Name+"/init.mp4"]+1))
							if err := errors.Join(readErr, cached.Close()); err != nil {
								return err
							}
							return fragment(jobCtx, name, w)
						}); err != nil {
							return err
						}
						var err error
						actual, err = p.probeFragment(jobCtx, path, r)
						return err
					}()
					err = errors.Join(err, removePackageScratch(path))
					if err != nil {
						fail(err)
						return
					}
					select {
					case results <- fragmentResult{index: index, probe: actual}:
					case <-jobCtx.Done():
						return
					}
				}
			}(worker)
		}
		go func() { workers.Wait(); close(results) }()
		for completed := range results {
			timeline[completed.index] = completed.probe
			objectDone++
			segmentDone++
			bytesDone += sizes[fmt.Sprintf("%s/seg-%06d.m4s", r.Name, completed.index)]
			emit("package_validation", r.Name)
		}
		cancel()
		if root != nil {
			return root
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateRenditionTimeline(r, timeline, reference); err != nil {
			return err
		}
		codecs[r.Name] = timeline[0].Codecs
		if reference == nil {
			reference = timeline
		}
		if err := os.Remove(initPath); err != nil {
			return err
		}
	}
	if err := assets.ValidateRecordingMasterCodecs(inv, master, codecs); err != nil {
		return fmt.Errorf("master codecs: %w", err)
	}
	emit("package_finalization", "")
	return nil
}

func checkFragmentScratch(dir string, required int64) error {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil {
		return err
	}
	if required <= 0 || required > 1<<30 || disk.Bsize <= 0 || uint64(required+(128<<20))/uint64(disk.Bsize)+1 > disk.Bavail {
		return errors.New("insufficient fragment scratch capacity")
	}
	return nil
}

func writePackageScratch(path string, write func(io.Writer) error) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	return errors.Join(write(file), file.Close())
}

func removePackageScratch(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func validateRenditionTimeline(r assets.RecordingRendition, timeline, reference []segmentProbe) error {
	if len(timeline) != r.SegmentCount || len(timeline) == 0 {
		return assets.ErrInvalidUpload
	}
	for i, actual := range timeline {
		if i == 0 {
			if actual.Start < -0.1 || actual.Start > 0.25 {
				return assets.ErrInvalidUpload
			}
		} else if math.Abs(actual.Start-timeline[i-1].End) > 1/r.FrameRate+0.001 || actual.Codecs != timeline[0].Codecs {
			return fmt.Errorf("segment decode: %w", assets.ErrInvalidUpload)
		}
		if i < r.SegmentCount-1 && math.Abs(actual.End-actual.Start-30) > 1/r.FrameRate+0.001 {
			return assets.ErrInvalidUpload
		}
	}
	if math.Abs(timeline[len(timeline)-1].End-timeline[0].Start-r.DurationSeconds) > 1/r.FrameRate+0.001 {
		return assets.ErrInvalidUpload
	}
	if reference != nil {
		if len(reference) != len(timeline) {
			return assets.ErrInvalidUpload
		}
		for i, actual := range timeline {
			if math.Abs(actual.Start-reference[i].Start) > 1/r.FrameRate+0.001 || math.Abs(actual.End-reference[i].End) > 1/r.FrameRate+0.001 {
				return assets.ErrInvalidUpload
			}
		}
	}
	return nil
}
