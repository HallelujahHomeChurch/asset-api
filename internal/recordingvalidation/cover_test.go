package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

func coverFFmpeg(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	return binary
}

func TestCustomCoverReencodesAndRemovesMetadata(t *testing.T) {
	var input bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	if err := png.Encode(&input, img); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	p := PackageMediaProbe{FFmpeg: coverFFmpeg(t), ScratchRoot: scratch}
	data, err := p.NormalizeCover(context.Background(), input.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || decoded.Bounds().Dx() != 1280 || decoded.Bounds().Dy() != 720 || len(data) > 1<<20 {
		t.Fatalf("invalid JPEG: %v", err)
	}
	r, g, b, _ := decoded.At(640, 360).RGBA()
	if r < 50000 || g > 5000 || b > 5000 {
		t.Fatal("source pixels lost")
	}
	var wrong bytes.Buffer
	if err := png.Encode(&wrong, image.NewGray(image.Rect(0, 0, 180, 320))); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NormalizeCover(context.Background(), wrong.Bytes(), "image/png"); err == nil {
		t.Fatal("portrait silently cropped")
	}
	if _, err := p.NormalizeCover(context.Background(), input.Bytes()[:40], "image/png"); err == nil {
		t.Fatal("truncated source accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.NormalizeCover(ctx, input.Bytes(), "image/png"); err == nil {
		t.Fatal("cancel ignored")
	}
	files, _ := os.ReadDir(scratch)
	if len(files) != 0 {
		t.Fatal("input scratch retained")
	}
}

func TestCoverCandidatesUseOnlyReadyFragmentsAndCleanup(t *testing.T) {
	ffmpeg := coverFFmpeg(t)
	dir := t.TempDir()
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=red:size=320x180:rate=30:duration=30", "-f", "lavfi", "-i", "color=c=lime:size=320x180:rate=30:duration=30", "-f", "lavfi", "-i", "color=c=blue:size=320x180:rate=30:duration=30", "-filter_complex", "[0:v][1:v][2:v]concat=n=3:v=1:a=0", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-g", "900", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	pkg, objects := packageTransferFixture()
	pkg.State = "ready"
	pkg.RecordingID = "recording-one"
	pkg.FinalPrefix = "recordings/packages/" + pkg.ID + "/final/validated/"
	pkg.Inventory.Renditions[0].DurationSeconds = 90
	pkg.Inventory.Renditions[0].SegmentCount = 3
	pkg.Inventory.Objects = append(pkg.Inventory.Objects, assets.RecordingPackageObject{Path: "720p/seg-000001.m4s"}, assets.RecordingPackageObject{Path: "720p/seg-000002.m4s"})
	for i, o := range pkg.Inventory.Objects {
		data := []byte("playlist")
		if !strings.HasSuffix(o.Path, ".m3u8") {
			var err error
			data, err = os.ReadFile(filepath.Join(dir, filepath.Base(o.Path)))
			if err != nil {
				t.Fatal(err)
			}
		}
		objects.bytes[pkg.FinalPrefix+o.Path] = data
		pkg.Inventory.Objects[i].SizeBytes = int64(len(data))
		pkg.Inventory.Objects[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	pkg.Inventory.InventoryDigest = ""
	scratch := t.TempDir()
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, ScratchRoot: scratch}
	writes := map[string][]byte{}
	put := func(_ context.Context, key string, data []byte) error { writes[key] = data; return nil }
	if err := probe.GenerateCovers(context.Background(), pkg, "cover-one", put); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 3 {
		t.Fatalf("got %d candidates", len(writes))
	}
	for i := 1; i <= 3; i++ {
		data := writes[fmt.Sprintf("recordings/covers/recording-one/cover-one/auto-%d.jpg", i)]
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 720 {
			t.Fatal("wrong cover size")
		}
		r, g, b, _ := img.At(640, 360).RGBA()
		channels := []uint32{r, g, b}
		for channel, value := range channels {
			if channel == i-1 && value < 40000 || channel != i-1 && value > 10000 {
				t.Fatalf("candidate %d wrong frame / blank nonzero PTS: %v", i, channels)
			}
		}
		if bytes.Contains(data, []byte("Exif")) {
			t.Fatal("metadata retained")
		}
	}
	files, _ := os.ReadDir(scratch)
	if len(files) != 0 {
		t.Fatal("scratch retained")
	}
	pkg.State = "validating"
	if err := probe.GenerateCovers(context.Background(), pkg, "cover-two", put); err == nil {
		t.Fatal("unvalidated media accepted")
	}
	pkg.State = "ready"
	pkg.FinalPrefix = "recordings/packages/other/final/validated/"
	if err := probe.GenerateCovers(context.Background(), pkg, "cover-two", put); err == nil {
		t.Fatal("cross-package read accepted")
	}
}

func TestCoverSampleTimes(t *testing.T) {
	for _, tc := range []struct {
		duration float64
		want     [3]float64
	}{{90, [3]float64{18, 45, 72}}, {0.01, [3]float64{0, 0, 0}}} {
		got := coverSampleTimes(tc.duration, 30)
		if got != tc.want {
			t.Fatalf("duration %v: %v", tc.duration, got)
		}
	}
}
