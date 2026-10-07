package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
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
	for _, rate := range []string{"30", "30000/1001", "mixed"} {
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
	for _, r := range assets.LiveRenditions() {
		dir := t.TempDir()
		profileRate := rate
		if rate == "mixed" {
			profileRate = "30"
			if r.Name == "480p" {
				profileRate = "30000/1001"
			}
		}
		cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("color=size=%dx%d:rate=%s", r.Width, r.Height, profileRate), "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "35", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "900", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8"))
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
	history, err := assets.AppendLiveSegment(nil, batch, false)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := probe.ValidateLiveSegment(context.Background(), p.ID, "claim-tail", 1, declarations[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = assets.AppendLiveSegment(history, tail, false); !errors.Is(err, assets.ErrCaptureMissingObjects) {
		t.Fatalf("unsealed tail: %v", err)
	}
	history, err = assets.AppendLiveSegment(history, tail, true)
	if err != nil {
		t.Fatal(err)
	}
	playlists, err := assets.RecordingLivePlaylists(history, true)
	expected := "FRAME-RATE=30.000"
	if rate != "30" {
		expected = "FRAME-RATE=29.970"
	}
	if err != nil || strings.Count(string(playlists["master.m3u8"]), expected) != 3 {
		t.Fatalf("master: %v %s", err, playlists["master.m3u8"])
	}
}
