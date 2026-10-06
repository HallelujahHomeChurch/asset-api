package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

func TestPreviewRealNonzeroTimestampAndPartialTail(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=red:size=320x180:rate=30", "-t", "36.2", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-g", "900", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	p, objects := packageTransferFixture()
	p.State = "ready"
	p.FinalPrefix = "recordings/packages/" + p.ID + "/final/validated/"
	p.Inventory.Renditions[0].DurationSeconds = 36.2
	p.Inventory.Renditions[0].SegmentCount = 2
	p.Inventory.Objects = append(p.Inventory.Objects, assets.RecordingPackageObject{Path: "720p/seg-000001.m4s"})
	for i, o := range p.Inventory.Objects {
		data := []byte("playlist")
		if !strings.HasSuffix(o.Path, ".m3u8") {
			data, err = os.ReadFile(filepath.Join(dir, filepath.Base(o.Path)))
			if err != nil {
				t.Fatal(err)
			}
		}
		objects.bytes[p.FinalPrefix+o.Path] = data
		p.Inventory.Objects[i].SizeBytes = int64(len(data))
		p.Inventory.Objects[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	p.Inventory.InventoryDigest = ""
	scratch := t.TempDir()
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: scratch}
	writes := map[string][]byte{}
	var order []string
	put := func(_ context.Context, key string, data []byte) error {
		writes[key] = data
		order = append(order, key)
		return nil
	}
	if err := probe.GeneratePreviews(context.Background(), p, "preview-one", put); err != nil {
		t.Fatal(err)
	}
	prefix := p.FinalPrefix + "previews/preview-one/"
	vtt := string(writes[prefix+"index.vtt"])
	if len(order) != 3 || order[2] != prefix+"index.vtt" {
		t.Fatalf("index not last: %v", order)
	}
	if strings.Count(vtt, " --> ") != 8 || !strings.Contains(vtt, "00:00:35.000 --> 00:00:36.200\nseg-000001.jpg#xywh=160,0,160,90") || strings.Contains(vtt, "seg-000001.jpg#xywh=320") {
		t.Fatalf("bad VTT: %s", vtt)
	}
	for _, name := range []string{"seg-000000.jpg", "seg-000001.jpg"} {
		image, err := jpeg.Decode(bytes.NewReader(writes[prefix+name]))
		if err != nil {
			t.Fatal(err)
		}
		cells := 6
		if name == "seg-000001.jpg" {
			cells = 2
		}
		for i := 0; i < cells; i++ {
			red, green, blue, _ := image.At(i*160+80, 45).RGBA()
			if red < 40000 || green > 10000 || blue > 10000 {
				t.Fatalf("empty/wrong sample at %s cell %d", name, i)
			}
		}
	}
	files, _ := os.ReadDir(scratch)
	if len(files) != 0 {
		t.Fatal("scratch retained")
	}
	p.FinalPrefix = "recordings/packages/other/final/validated/"
	if err := probe.GeneratePreviews(context.Background(), p, "preview-one", put); err == nil {
		t.Fatal("cross-package read accepted")
	}
}

func TestPreviewCellsDoNotExposeDurationRoundingPadding(t *testing.T) {
	for _, tc := range []struct {
		data     string
		duration float64
		want     int
	}{
		{`{"packets":[{"pts_time":"30"},{"pts_time":"34.966667"}]}`, 5.01, 1},
		{`{"packets":[{"pts_time":"30"},{"pts_time":"35.000000"}]}`, 5.04, 2},
		{`{"packets":[{"pts_time":"0"},{"pts_time":"29.966667"}]}`, 30, 6},
	} {
		got, err := previewCells([]byte(tc.data), tc.duration)
		if err != nil || got != tc.want {
			t.Fatalf("cells %d want %d: %v", got, tc.want, err)
		}
	}
}
