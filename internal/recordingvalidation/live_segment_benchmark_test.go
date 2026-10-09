package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

// Same 29.97 fps moving-media fixture and tools for before/after measurements.
// Provider latency is controlled; this does not model production R2 throughput.
func BenchmarkLiveSegmentValidation(b *testing.B) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		b.Fatal(err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		b.Fatal(err)
	}
	p, objects := packageTransferFixture()
	var declarations []assets.RecordingPackageObject
	var mediaBytes int64
	for _, r := range assets.LiveRenditions() {
		dir := b.TempDir()
		args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=30000/1001", r.Width, r.Height), "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "30.03", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-b:v", fmt.Sprint(r.VideoBitrate), "-maxrate", fmt.Sprint(r.VideoBitrate), "-bufsize", fmt.Sprint(2 * r.VideoBitrate), "-g", "900", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", "30.03", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			b.Fatalf("fixture: %v %s", err, out)
		}
		for _, name := range []string{"init.mp4", "seg-000000.m4s"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				b.Fatal(err)
			}
			hash := sha256.Sum256(data)
			path := r.Name + "/" + name
			objects.bytes[p.StagingKey(path)] = data
			declarations = append(declarations, assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
			mediaBytes += int64(len(data))
		}
	}
	for _, latency := range []time.Duration{0, 50 * time.Millisecond} {
		b.Run(fmt.Sprintf("provider_%dms", latency.Milliseconds()), func(b *testing.B) {
			probe := PackageMediaProbe{Objects: latencyLiveObjects{PackageObjects: objects, latency: latency}, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: b.TempDir()}
			b.SetBytes(mediaBytes)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				batch, err := probe.ValidateLiveSegment(context.Background(), p.ID, fmt.Sprintf("bench-%d", i), 0, declarations)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := assets.AppendLiveSegment(nil, batch, false); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(30.03*float64(b.N)/b.Elapsed().Seconds(), "media_s/wall_s")
		})
	}
}

type latencyLiveObjects struct {
	PackageObjects
	latency time.Duration
}

func (s latencyLiveObjects) pause(ctx context.Context) error {
	timer := time.NewTimer(s.latency)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s latencyLiveObjects) Head(ctx context.Context, key string) (int64, string, error) {
	if err := s.pause(ctx); err != nil {
		return 0, "", err
	}
	return s.PackageObjects.Head(ctx, key)
}
func (s latencyLiveObjects) CopyPackageObject(ctx context.Context, from, to, etag string) error {
	if err := s.pause(ctx); err != nil {
		return err
	}
	return s.PackageObjects.CopyPackageObject(ctx, from, to, etag)
}
func (s latencyLiveObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := s.pause(ctx); err != nil {
		return nil, err
	}
	return s.PackageObjects.Open(ctx, key)
}
