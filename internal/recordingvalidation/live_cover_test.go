package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hhc/asset-api/internal/assets"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveCoverUsesFirstVerifiedFragmentAndSkipsBlack(t *testing.T) {
	ffmpeg := coverFFmpeg(t)
	dir := t.TempDir()
	args := []string{"-v", "error", "-f", "lavfi", "-i", "color=c=black:size=320x180:rate=30000/1001:duration=0.4", "-f", "lavfi", "-i", "color=c=lime:size=320x180:rate=30000/1001:duration=0.6", "-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-g", "900", "-f", "hls", "-hls_time", "30.03", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	_, objects := packageTransferFixture()
	capture := strings.Repeat("a", 32)
	var media []assets.RecordingPackageObject
	for _, name := range []string{"init.mp4", "seg-000000.m4s"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		path := "480p/" + name
		objects.bytes["recordings/captures/"+capture+"/final/"+path] = data
		media = append(media, assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	scratch := t.TempDir()
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, ScratchRoot: scratch}
	data, err := probe.GenerateLiveCover(context.Background(), capture, 1.001, media)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 720 {
		t.Fatal("invalid JPEG", err)
	}
	r, g, b, _ := img.At(640, 360).RGBA()
	if g < 40000 || r > 10000 || b > 10000 {
		t.Fatal("selected black/incorrect frame", r, g, b)
	}
	files, _ := os.ReadDir(scratch)
	if len(files) != 0 {
		t.Fatal("retained scratch")
	}
	if _, err := probe.GenerateLiveCover(context.Background(), capture, .1, media); err == nil {
		t.Fatal("accepted only black frames")
	}
	media[1].SHA256 = strings.Repeat("0", 64)
	if _, err = probe.GenerateLiveCover(context.Background(), capture, 1.001, media); err == nil {
		t.Fatal("corrupt stable media accepted")
	}
	if _, err = probe.GenerateLiveCover(context.Background(), "../other", 1.001, media); err == nil {
		t.Fatal("cross-capture path accepted")
	}
}
