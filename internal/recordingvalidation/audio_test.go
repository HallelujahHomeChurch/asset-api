package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hhc/asset-api/internal/assets"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAudioRemuxPreservesTimelineWithoutSourceUpload(t *testing.T) {
	for _, seconds := range []int{30, 10} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) { testAudioRemux(t, seconds) })
	}
}
func testAudioRemux(t *testing.T, seconds int) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "31.2", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-g", strconv.Itoa(seconds * 30), "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", strconv.Itoa(seconds), "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
	if data, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, data)
	}
	pkg, objects := packageTransferFixture()
	pkg.State = "ready"
	pkg.FinalPrefix = "recordings/packages/" + pkg.ID + "/final/validated/"
	pkg.Inventory.Renditions[0].DurationSeconds = 31.2
	segments := 2
	if seconds == 10 {
		segments = 4
		pkg.Inventory.PresetVersion = assets.RecordingTenSecondPreset
		pkg.Inventory.Renditions[0].FrameRate = 30000.0 / 1001
	}
	pkg.Inventory.Renditions[0].SegmentCount = segments
	extra := pkg.Inventory.Objects[len(pkg.Inventory.Objects)-1]
	for i := 1; i < segments; i++ {
		extra.Path = fmt.Sprintf("720p/seg-%06d.m4s", i)
		pkg.Inventory.Objects = append(pkg.Inventory.Objects, extra)
	}
	for i, o := range pkg.Inventory.Objects {
		data := []byte("playlist")
		if !strings.HasSuffix(o.Path, ".m3u8") {
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
	writes := map[string][]byte{}
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: t.TempDir()}
	err = probe.GenerateAudio(context.Background(), pkg, "attempt", func(_ context.Context, key string, b []byte) error { writes[key] = b; return nil })
	if err != nil {
		t.Fatal(err)
	}
	prefix := pkg.FinalPrefix + "audio/aac-hls-v1/attempt/"
	if len(writes) != segments+1 || !strings.Contains(string(writes[prefix+"index.m3u8"]), fmt.Sprintf("seg-%06d.ts\n#EXT-X-ENDLIST", segments-1)) {
		t.Fatal("incomplete audio manifest")
	}
	for i := 0; i < segments; i++ {
		file := filepath.Join(dir, fmt.Sprintf("audio%d.ts", i))
		if err := os.WriteFile(file, writes[prefix+fmt.Sprintf("seg-%06d.ts", i)], 0600); err != nil {
			t.Fatal(err)
		}
		data, err := ProbeCommand(context.Background(), ffprobe, "-v", "error", "-show_streams", "-show_packets", "-show_entries", "stream=codec_name,codec_type,profile,sample_rate:packet=pts_time,duration_time", "-of", "json", file)
		if err != nil {
			t.Fatal(err)
		}
		start, _, err := audioBounds(data)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && start < float64(seconds) {
			t.Fatalf("timestamp reset: %f", start)
		}
	}

	hlsDir := t.TempDir()
	for key, data := range writes {
		if err := os.WriteFile(filepath.Join(hlsDir, filepath.Base(key)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file,crypto,data", "-i", filepath.Join(hlsDir, "index.m3u8"), "-map", "0:a:0", "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("complete audio HLS decode: %v %s", err, output)
	}
	objects.bytes[pkg.FinalPrefix+"720p/seg-000000.m4s"][0] ^= 1
	if err := probe.GenerateAudio(context.Background(), pkg, "corrupt", func(context.Context, string, []byte) error { t.Fatal("published corrupted source"); return nil }); err == nil {
		t.Fatal("accepted source hash mismatch")
	}
}
