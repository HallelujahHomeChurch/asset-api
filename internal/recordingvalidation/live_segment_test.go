package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveSegmentRejectsMissingOrChangedObject(t *testing.T) {
	p, objects := packageTransferFixture()
	probe := PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe", ScratchRoot: t.TempDir()}
	if _, err := probe.ValidateLiveSegment(context.Background(), p.ID, "claim", 0, p.Inventory.Objects); !errors.Is(err, assets.ErrCaptureMissingObjects) {
		t.Fatalf("missing init: %v", err)
	}
	declarations := []assets.RecordingPackageObject{}
	for _, r := range assets.LiveRenditions() {
		for _, name := range []string{"init.mp4", "seg-000000.m4s"} {
			o := p.Inventory.Objects[0]
			o.Path = r.Name + "/" + name
			o.SizeBytes = 3
			o.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			declarations = append(declarations, o)
			objects.bytes[p.StagingKey(o.Path)] = []byte("bad")
		}
	}
	if _, err := probe.ValidateLiveSegment(context.Background(), p.ID, "claim", 0, declarations); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("changed bytes: %v", err)
	}
}

func TestLiveSegmentDecodesThreeProfilesAndNormalTail(t *testing.T) {
	for _, rate := range []string{"30", "30000/1001", "30000/1001-full", "30000/1001-short", "mixed"} {
		t.Run(rate, func(t *testing.T) { testLiveSegmentFrameRate(t, rate) })
	}
}
func testLiveSegmentFrameRate(t *testing.T, rate string) {
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(e)
		}
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, e := exec.LookPath("ffprobe")
	if e != nil {
		t.Fatal(e)
	}
	p, objects := packageTransferFixture()
	declarations := map[int][]assets.RecordingPackageObject{0: {}, 1: {}}
	duration := "35"
	frames, segmentTime, profile := "900", "30", 900
	if rate == "30000/1001-short" {
		rate, duration, frames, segmentTime, profile = "30000/1001", "15", "300", "10.01", 300
	}
	if rate == "30000/1001-full" {
		rate, duration = "30000/1001", "60.06"
	}
	for _, r := range assets.LiveRenditions() {
		dir := t.TempDir()
		profileRate := rate
		if rate == "mixed" {
			profileRate = "30"
			if r.Name == "480p" {
				profileRate = "30000/1001"
			}
		}
		cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("color=size=%dx%d:rate=%s", r.Width, r.Height, profileRate), "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", duration, "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", frames, "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", segmentTime, "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
		for seq := 0; seq < 2; seq++ {
			for _, name := range []string{"init.mp4", fmt.Sprintf("seg-%06d.m4s", seq)} {
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(b)
				path := r.Name + "/" + name
				objects.bytes[p.StagingKey(path)] = b
				declarations[seq] = append(declarations[seq], assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(b)), SHA256: hex.EncodeToString(hash[:])})
			}
		}
	}
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: t.TempDir()}
	batch, err := probe.ValidateLiveSegment(context.Background(), p.ID, "claim-first", 0, declarations[0])
	if rate == "mixed" {
		if !errors.Is(err, assets.ErrInvalidUpload) {
			t.Fatalf("mixed real-media fps: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	history, err := assets.AppendLiveSegment(nil, batch, false, profile)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := probe.ValidateLiveSegment(context.Background(), p.ID, "claim-tail", 1, declarations[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = assets.AppendLiveSegment(history, tail, false, profile); (duration == "35" || profile == 300) && !errors.Is(err, assets.ErrCaptureMissingObjects) {
		t.Fatalf("unsealed tail: %v", err)
	}
	history, err = assets.AppendLiveSegment(history, tail, true, profile)
	if err != nil {
		t.Fatal(err)
	}
	for index, segment := range history {
		for name, fragment := range segment.Renditions {
			receipt := fragment.Validation
			if receipt == nil || receipt.Version != liveValidationVersion || receipt.Init.Path != name+"/init.mp4" || receipt.Segment.Path != fmt.Sprintf("%s/seg-%06d.m4s", name, index) {
				t.Fatalf("missing bound validation evidence: %+v", receipt)
			}
		}
	}
	playlists, err := assets.RecordingLivePlaylists(history, true, profile)
	expected := "FRAME-RATE=30.000"
	if rate != "30" {
		expected = "FRAME-RATE=29.970"
	}
	if err != nil || strings.Count(string(playlists["master.m3u8"]), expected) != 3 {
		t.Fatalf("master: %v %s", err, playlists["master.m3u8"])
	}
	if profile == 300 {
		p.Inventory.PresetVersion = assets.RecordingTenSecondPreset
	}
	p.Inventory.Objects, p.Inventory.Renditions = nil, nil
	master := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n"
	for _, r := range assets.LiveRenditions() {
		r.FrameRate = history[0].Renditions[r.Name].FrameRate
		r.DurationSeconds = history[1].Renditions[r.Name].End - history[0].Renditions[r.Name].Start
		r.SegmentCount = 2
		p.Inventory.Renditions = append(p.Inventory.Renditions, r)
		sizes := []int64{int64(len(objects.bytes[p.StagingKey(r.Name+"/seg-000000.m4s")])), int64(len(objects.bytes[p.StagingKey(r.Name+"/seg-000001.m4s")]))}
		durations := []float64{history[0].Renditions[r.Name].End - history[0].Renditions[r.Name].Start, history[1].Renditions[r.Name].End - history[1].Renditions[r.Name].Start}
		target := 31
		if profile == 300 {
			target = 11
		}
		peak, average, err := assets.RecordingPlaylistBitrates(sizes, durations, target)
		if err != nil {
			t.Fatal(err)
		}
		master += fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%.3f,CODECS=\"%s\"\n%s/index.m3u8\n", peak, average, r.Width, r.Height, r.FrameRate, history[0].Renditions[r.Name].Codecs, r.Name)
	}
	playlists["master.m3u8"] = []byte(master)
	for path, data := range playlists {
		objects.bytes[p.StagingKey(path)] = []byte(strings.Replace(string(data), "#EXT-X-PLAYLIST-TYPE:EVENT", "#EXT-X-PLAYLIST-TYPE:VOD", 1))
	}
	for key, data := range objects.bytes {
		if !strings.HasPrefix(key, p.StagingKey("")) {
			continue
		}
		hash := sha256.Sum256(data)
		p.Inventory.Objects = append(p.Inventory.Objects, assets.RecordingPackageObject{Path: strings.TrimPrefix(key, p.StagingKey("")), SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
	}
	p.Inventory.InventoryDigest, err = assets.RecordingInventoryDigest(p.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	p.SizeBytes, err = assets.ValidateRecordingInventory(p.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FreezeRecordingPackage(context.Background(), p, "final-seal", objects, probe); err != nil {
		t.Fatalf("final sealed three-profile package: %v", err)
	}
	if err := validateLiveArchiveEvidence(p.Inventory, playlists["master.m3u8"], history); err != nil {
		t.Fatalf("live and full validation disagree: %v", err)
	}
	for name, mutate := range map[string]func(*assets.RecordingLiveFragment){
		"legacy":       func(f *assets.RecordingLiveFragment) { f.Validation = nil },
		"version":      func(f *assets.RecordingLiveFragment) { f.Validation.Version = "future" },
		"init-hash":    func(f *assets.RecordingLiveFragment) { f.Validation.Init.SHA256 = strings.Repeat("0", 64) },
		"segment-hash": func(f *assets.RecordingLiveFragment) { f.Validation.Segment.SHA256 = strings.Repeat("0", 64) },
		"segment-size": func(f *assets.RecordingLiveFragment) { f.Validation.Segment.SizeBytes++ },
		"segment-path": func(f *assets.RecordingLiveFragment) { f.Validation.Segment.Path = "480p/seg-000999.m4s" },
		"frame-rate":   func(f *assets.RecordingLiveFragment) { f.FrameRate = 24 },
		"nan":          func(f *assets.RecordingLiveFragment) { f.Start = math.NaN() },
		"infinite":     func(f *assets.RecordingLiveFragment) { f.End = math.Inf(1) },
		"timeline":     func(f *assets.RecordingLiveFragment) { f.Start += .5 },
		"codec":        func(f *assets.RecordingLiveFragment) { f.Codecs = "avc1.000000,mp4a.40.2" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			var changed []assets.RecordingLiveSegment
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			fragment := changed[0].Renditions["480p"]
			mutate(&fragment)
			changed[0].Renditions["480p"] = fragment
			if err := validateLiveArchiveEvidence(p.Inventory, playlists["master.m3u8"], changed); err == nil {
				t.Fatal("accepted incompatible live evidence")
			}
		})
	}
}
