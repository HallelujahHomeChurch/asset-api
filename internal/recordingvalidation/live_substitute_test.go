package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type substituteTestStore struct {
	*liveWorkerObjects
	dir string
}

func (s substituteTestStore) PutLiveSubstitute(_ context.Context, _ string, r string, n int, b []byte) error {
	return os.WriteFile(filepath.Join(s.dir, r, fmt.Sprintf("sub-seg-%06d.m4s", n)), b, 0600)
}

func TestLiveSubstitutePlayback(t *testing.T) {
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
	if export := os.Getenv("HHC_PLAYBACK_FIXTURE"); export != "" {
		if err := os.Mkdir(export, 0700); err != nil {
			t.Fatal(err)
		}
		dir = export
	}
	_, objects := packageTransferFixture()
	store := substituteTestStore{&liveWorkerObjects{packageMemoryObjects: objects, passThrough: true}, dir}
	probe := PackageMediaProbe{Objects: store, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: dir}
	id := "fixture"
	var history []assets.RecordingLiveSegment
	batches := [3]map[string]assets.RecordingLiveFragment{{}, {}, {}}
	for _, r := range assets.LiveRenditions() {
		path := filepath.Join(dir, r.Name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=30000/1001", r.Width, r.Height), "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "30.03", "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-bf", "0", "-g", "300", "-sc_threshold", "0", "-ac", "2", "-c:a", "aac", "-f", "hls", "-hls_time", "10.01", "-hls_segment_type", "fmp4", "-hls_segment_filename", "seg-%06d.m4s", "fixture.m3u8")
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
		evidence := func(name string) assets.RecordingPackageObject {
			b, err := os.ReadFile(filepath.Join(path, name))
			if err != nil {
				t.Fatal(err)
			}
			key := r.Name + "/" + name
			objects.bytes["recordings/captures/"+id+"/final/"+key] = b
			return assets.RecordingPackageObject{Path: key, SizeBytes: int64(len(b)), SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
		}
		init := evidence("init.mp4")
		for n := 0; n < 3; n++ {
			name := fmt.Sprintf("seg-%06d.m4s", n)
			if n == 1 && r.Name == "480p" {
				if err := os.Remove(filepath.Join(path, name)); err != nil {
					t.Fatal(err)
				}
				continue
			}
			seg := evidence(name)
			batches[n][r.Name] = assets.RecordingLiveFragment{FrameRate: 30000.0 / 1001, VideoFrames: 300, Start: 0.021 + float64(n)*10.01, End: 0.021 + float64(n+1)*10.01, Codecs: "avc1.640020,mp4a.40.2", Validation: &assets.RecordingLiveValidation{Init: init, Segment: seg}}
		}
	}
	start := time.Now()
	for n, b := range batches {
		if n == 1 {
			prepareLiveSubstitutes(t.Context(), store, probe, postgres.RecordingLiveClaim{Capture: assets.RecordingCapture{ID: id}, Sequence: n, Snapshot: postgres.RecordingLiveSnapshot{Segments: history}}, b)
			if !b["480p"].Substitute {
				t.Fatal("no verified same-profile substitute")
			}
		}
		history, err = assets.AppendAvailableLiveSegment(history, b, false, 300)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("substitute ready after %s", time.Since(start))
	playlists, err := assets.RecordingLivePlaylists(history, true, 300)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range playlists {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, track := range []string{"v:0", "a:0"} {
		cmd := exec.Command(ffprobe, "-v", "error", "-allowed_extensions", "ALL", "-select_streams", track, "-show_entries", "frame=best_effort_timestamp_time,width,height,nb_samples", "-of", "csv=p=0", filepath.Join(dir, "480p", "index.m3u8"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		var prev float64
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			r, err := csv.NewReader(strings.NewReader(line)).Read()
			if err != nil {
				t.Fatal(err)
			}
			pts, err := strconv.ParseFloat(r[0], 64)
			if err != nil {
				t.Fatalf("decoder diagnostic: %s", line)
			}
			step := 1024.0 / 48000
			if track == "v:0" {
				step = 1001.0 / 30000
				if len(r) < 3 || r[1] != "854" || r[2] != "480" {
					t.Fatalf("wrong frame: %v", r)
				}
			}
			if count > 0 && math.Abs(pts-prev-step) > 0.00001 {
				t.Fatalf("%s timestamp jumped from %f to %f", track, prev, pts)
			}
			prev = pts
			count++
		}
		if track == "v:0" && count != 900 || track == "a:0" && count < 1400 {
			t.Fatalf("%s decoded %d", track, count)
		}
	}
}
