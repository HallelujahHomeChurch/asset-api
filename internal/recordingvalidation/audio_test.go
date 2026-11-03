package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"log/slog"
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
	var logs bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(originalLogger)

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
	var inputBytes, outputBytes int64
	for i := 0; i < segments; i++ {
		inputBytes += int64(len(objects.bytes[pkg.FinalPrefix+"720p/init.mp4"]) + len(objects.bytes[pkg.FinalPrefix+fmt.Sprintf("720p/seg-%06d.m4s", i)]))
	}
	for _, data := range writes {
		outputBytes += int64(len(data))
	}
	assertAudioLog(t, logs.Bytes(), pkg.RecordingID, true, inputBytes, outputBytes, segments)

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

	logs.Reset()
	puts := 0
	var firstOutput int64
	err = probe.GenerateAudio(context.Background(), pkg, "attempt", func(_ context.Context, _ string, data []byte) error {
		puts++
		if puts == 2 {
			return errors.New("https://private.invalid/key?token=do-not-log")
		}
		firstOutput = int64(len(data))
		return nil
	})
	if err == nil || puts != 2 {
		t.Fatalf("expected second PUT failure: %d %v", puts, err)
	}
	partialInput := int64(2*len(objects.bytes[pkg.FinalPrefix+"720p/init.mp4"]) + len(objects.bytes[pkg.FinalPrefix+"720p/seg-000000.m4s"]) + len(objects.bytes[pkg.FinalPrefix+"720p/seg-000001.m4s"]))
	assertAudioLog(t, logs.Bytes(), pkg.RecordingID, false, partialInput, firstOutput, 1)
	objects.bytes[pkg.FinalPrefix+"720p/seg-000000.m4s"][0] ^= 1
	if err := probe.GenerateAudio(context.Background(), pkg, "corrupt", func(context.Context, string, []byte) error { t.Fatal("published corrupted source"); return nil }); err == nil {
		t.Fatal("accepted source hash mismatch")
	}
}

func assertAudioLog(t *testing.T, data []byte, recordingID string, success bool, input, output int64, segments int) {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["msg"] != "recording_audio_generate" || entry["claim_id"] != "attempt" || entry["recording_id"] != recordingID || entry["success"] != success || entry["input_bytes"] != float64(input) || entry["output_bytes"] != float64(output) || entry["segments"] != float64(segments) {
		t.Fatalf("unexpected aggregate log: %v", entry)
	}
	if ms, ok := entry["elapsed_ms"].(float64); !ok || ms < 0 {
		t.Fatal("missing elapsed time")
	}
	for key := range entry {
		switch key {
		case "time", "level", "msg", "claim_id", "recording_id", "input_bytes", "output_bytes", "segments", "elapsed_ms", "success":
		default:
			t.Fatalf("unexpected log field %s", key)
		}
	}
	if bytes.Contains(data, []byte("private.invalid")) || bytes.Contains(data, []byte("do-not-log")) || bytes.Contains(data, []byte("recordings/packages/")) {
		t.Fatal("sensitive media data leaked into log")
	}
}
