package recordingvalidation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
)

// NormalizeCover runs only in the bounded recording worker, never the API.
// Source bytes and decoder metadata are discarded, not served to viewers.
func (p PackageMediaProbe) NormalizeCover(ctx context.Context, data []byte, mime string) (result []byte, err error) {
	cfg, err := assets.ValidateRecordingCoverInput(data, mime)
	if err != nil {
		return nil, err
	}
	if cfg.Width*9 != cfg.Height*16 || !filepath.IsAbs(p.FFmpeg) {
		return nil, assets.ErrInvalidUpload
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-cover-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	source := filepath.Join(dir, "input")
	if err := os.WriteFile(source, data, 0600); err != nil {
		return nil, err
	}
	demuxer := "jpeg_pipe"
	if mime == "image/png" {
		demuxer = "png_pipe"
	}
	return p.coverFrame(ctx, source, "scale=1280:720", demuxer)
}

func coverSampleTimes(duration, fps float64) [3]float64 {
	last := math.Max(0, duration-1/fps)
	return [3]float64{math.Min(duration*.2, last), math.Min(duration*.5, last), math.Min(duration*.8, last)}
}

// GenerateCovers reads a bounded init+fragment for each candidate, never the
// original upload or a mutable staging object. Caller fences all DB publication.
func (p PackageMediaProbe) GenerateCovers(ctx context.Context, pkg assets.RecordingPackage, attempt string, put func(context.Context, string, []byte) error) (err error) {
	return p.GenerateCoversWithTimeline(ctx, pkg, attempt, nil, nil, put)
}

func (p PackageMediaProbe) GenerateCoversWithTimeline(ctx context.Context, pkg assets.RecordingPackage, attempt string, inherited []byte, timeline []assets.RecordingLiveSegment, put func(context.Context, string, []byte) error) (err error) {
	if p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || pkg.State != "ready" || !packageIDPattern.MatchString(pkg.ID) || !packageIDPattern.MatchString(pkg.RecordingID) || !packageIDPattern.MatchString(attempt) || put == nil {
		return assets.ErrInvalidInput
	}
	parts := strings.Split(pkg.FinalPrefix, "/")
	if len(parts) != 6 || parts[0] != "recordings" || parts[1] != "packages" || parts[2] != pkg.ID || parts[3] != "final" || !packageIDPattern.MatchString(parts[4]) || parts[5] != "" {
		return assets.ErrInvalidInput
	}
	if _, err := assets.ValidateRecordingInventory(pkg.Inventory); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rendition := pkg.Inventory.Renditions[0]
	for _, r := range pkg.Inventory.Renditions {
		if r.Width < rendition.Width {
			rendition = r
		}
	}
	sizes := map[string]int64{}
	for _, o := range pkg.Inventory.Objects {
		sizes[o.Path] = o.SizeBytes
	}
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-cover-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	duration := rendition.DurationSeconds
	if len(timeline) > 0 {
		duration = timeline[len(timeline)-1].Renditions[rendition.Name].End - timeline[0].Renditions[rendition.Name].Start
	}
	for i, seconds := range coverSampleTimes(duration, rendition.FrameRate) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i == 0 && len(inherited) > 0 {
			if len(inherited) > assets.RecordingCoverOutputMaxBytes {
				return assets.ErrInvalidUpload
			}
			if err := put(ctx, fmt.Sprintf("recordings/covers/%s/%s/auto-1.jpg", pkg.RecordingID, attempt), inherited); err != nil {
				return err
			}
			continue
		}
		segment, offset, positionErr := coverPosition(seconds, rendition.Name, timeline)
		if positionErr != nil {
			return positionErr
		}
		names := []string{rendition.Name + "/init.mp4", fmt.Sprintf("%s/seg-%06d.m4s", rendition.Name, segment)}
		var disk syscall.Statfs_t
		if err := syscall.Statfs(dir, &disk); err != nil {
			return err
		}
		required := int64(64 << 20)
		for _, name := range names {
			size := sizes[name]
			if size <= 0 || size > assets.RecordingObjectMaxBytes {
				return assets.ErrInvalidUpload
			}
			required += size
		}
		if disk.Bsize <= 0 || uint64(required)/uint64(disk.Bsize)+1 > disk.Bavail {
			return errors.New("insufficient cover scratch capacity")
		}
		source := filepath.Join(dir, "fragment.mp4")
		file, err := os.OpenFile(source, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		copyErr := func() error {
			for _, name := range names {
				body, err := p.Objects.Open(ctx, pkg.FinalPrefix+name)
				if err != nil {
					return err
				}
				n, err := io.Copy(file, io.LimitReader(body, sizes[name]+1))
				err = errors.Join(err, body.Close())
				if err != nil {
					return err
				}
				if n != sizes[name] {
					return assets.ErrInvalidUpload
				}
			}
			return nil
		}()
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
		filter := fmt.Sprintf("setpts=PTS-STARTPTS,select='gte(t,%.6f)',scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2", offset)
		data, err := p.coverFrame(ctx, source, filter, "mov")
		if err != nil {
			return err
		}
		if err := put(ctx, fmt.Sprintf("recordings/covers/%s/%s/auto-%d.jpg", pkg.RecordingID, attempt, i+1), data); err != nil {
			return err
		}
		if err := os.Remove(source); err != nil {
			return err
		}
	}
	return nil
}

func (p PackageMediaProbe) coverFrame(ctx context.Context, source, filter, demuxer string) ([]byte, error) {
	args := []string{"-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file,pipe", "-f", demuxer}
	if demuxer == "mov" {
		args = append(args, "-enable_drefs", "0", "-use_absolute_path", "0")
	}
	args = append(args, "-threads", "2", "-i", source, "-map", "0:v:0", "-an", "-sn", "-dn", "-vf", filter, "-frames:v", "1", "-map_metadata", "-1", "-c:v", "png", "-threads", "1", "-f", "image2pipe", "pipe:1")
	data, err := ProbeCommand(ctx, p.FFmpeg, args...)
	if err != nil {
		return nil, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 1280 || cfg.Height != 720 {
		return nil, assets.ErrInvalidUpload
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, assets.ErrInvalidUpload
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	if output.Len() > assets.RecordingCoverOutputMaxBytes {
		return nil, assets.ErrInvalidUpload
	}
	return output.Bytes(), nil
}

// Captures use their measured segment boundaries; older package-only uploads retain their existing schedule.
func coverPosition(seconds float64, rendition string, timeline []assets.RecordingLiveSegment) (int, float64, error) {
	if len(timeline) == 0 {
		segment := int(seconds / 30)
		return segment, math.Max(0, seconds-float64(segment*30)), nil
	}
	first, ok := timeline[0].Renditions[rendition]
	if !ok {
		return 0, 0, assets.ErrInvalidUpload
	}
	target := first.Start + seconds
	for _, segment := range timeline {
		fragment, ok := segment.Renditions[rendition]
		if !ok {
			return 0, 0, assets.ErrInvalidUpload
		}
		if target >= fragment.Start && target < fragment.End {
			return segment.Sequence, target - fragment.Start, nil
		}
	}
	return 0, 0, assets.ErrInvalidUpload
}
