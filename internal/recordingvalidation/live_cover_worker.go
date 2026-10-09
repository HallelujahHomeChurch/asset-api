package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Only a published, verified init and first fragment may feed the optional cover.
func (p PackageMediaProbe) GenerateLiveCover(ctx context.Context, capture string, duration float64, media []assets.RecordingPackageObject) ([]byte, error) {
	if !liveCoverCaptureID(capture) || p.Objects == nil || !filepath.IsAbs(p.FFmpeg) || len(media) != 2 || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return nil, assets.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(p.ScratchRoot, "hhc-live-cover-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil {
		return nil, err
	}
	required := int64(64 << 20)
	for _, object := range media {
		if object.SizeBytes <= 0 || object.SizeBytes > assets.RecordingObjectMaxBytes {
			return nil, assets.ErrInvalidInput
		}
		required += object.SizeBytes
	}
	if disk.Bsize <= 0 || uint64(required)/uint64(disk.Bsize)+1 > disk.Bavail {
		return nil, errors.New("insufficient live cover scratch capacity")
	}
	source := filepath.Join(dir, "source.mp4")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	for i, name := range []string{"480p/init.mp4", "480p/seg-000000.m4s"} {
		object := media[i]
		if object.Path != name || object.SizeBytes <= 0 || object.SizeBytes > assets.RecordingObjectMaxBytes {
			file.Close()
			return nil, assets.ErrInvalidInput
		}
		body, e := p.Objects.Open(ctx, "recordings/captures/"+capture+"/final/"+name)
		if e != nil {
			file.Close()
			return nil, e
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, object.SizeBytes+1))
		e = errors.Join(e, body.Close())
		if e != nil {
			file.Close()
			return nil, e
		}
		if n != object.SizeBytes || fmt.Sprintf("%x", hash.Sum(nil)) != object.SHA256 {
			file.Close()
			return nil, assets.ErrInvalidUpload
		}
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	for _, seconds := range []float64{0, .5, 1, 2, 4} {
		if seconds >= duration {
			continue
		}
		filter := fmt.Sprintf("setpts=PTS-STARTPTS,select='gte(t,%.6f)',scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2", seconds)
		data, e := p.coverFrame(ctx, source, filter, "mov")
		if e != nil {
			return nil, e
		}
		img, e := jpeg.Decode(bytes.NewReader(data))
		if e != nil {
			return nil, e
		}
		dark, total := 0, 0
		for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
			for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
				r, g, b, _ := img.At(x, y).RGBA()
				total++
				if r <= 16*257 && g <= 16*257 && b <= 16*257 {
					dark++
				}
			}
		}
		if dark*100 < total*99 {
			return data, nil
		}
	}
	return nil, assets.ErrInvalidUpload
}
func liveCoverCaptureID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func RunLiveCoverProcessing(ctx context.Context, repo *postgres.RecordingLiveCoverStore, objects CoverObjects, probe PackageMediaProbe) (bool, error) {
	c, err := repo.Claim(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	var data []byte
	if c.Kind == "auto" {
		var duration float64
		var media []assets.RecordingPackageObject
		duration, media, err = repo.AutoMedia(work, c)
		if err == nil {
			data, err = probe.GenerateLiveCover(work, c.Scope, duration, media)
		}
	} else {
		var body io.ReadCloser
		body, err = objects.Open(work, c.InputKey())
		if err == nil {
			var input []byte
			input, err = io.ReadAll(io.LimitReader(body, assets.RecordingCoverInputMaxBytes+1))
			err = errors.Join(err, body.Close())
			if err == nil && fmt.Sprintf("%x", sha256.Sum256(input)) != c.Digest {
				err = assets.ErrInvalidUpload
			}
			if err == nil {
				data, err = probe.NormalizeCover(work, input, c.MIME)
			}
		}
	}
	if err == nil {
		c.OutputAttempt = c.ClaimID
		err = objects.PutRecordingCover(work, c.Key(), data)
	}
	cancel()
	finish, finishCancel := context.WithTimeout(ctx, 15*time.Second)
	defer finishCancel()
	finishErr := repo.Finish(finish, c, fmt.Sprintf("%x", sha256.Sum256(data)), err == nil)
	if err == nil && finishErr == nil && c.Kind == "custom" {
		finishErr = objects.DeleteCoverObjects(finish, []string{c.InputKey()})
	}
	return true, errors.Join(err, finishErr)
}
