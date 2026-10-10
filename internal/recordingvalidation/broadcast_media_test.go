package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hhc/asset-api/internal/assets"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testsrc burns elapsed capture seconds into the image. Yellow/green/red bars
// distinguish rehearsal [0,8), public [8,20), and post-service [20,24).
func TestBroadcastRealDecodedRangeAndDerivatives(t *testing.T) {
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
	ctx := context.Background()
	root := t.TempDir()
	if output := os.Getenv("HHC_BROADCAST_FIXTURE_DIR"); output != "" {
		root = output
		if err = os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	p, objects := packageTransferFixture()
	p.ID = strings.Repeat("a", 32)
	p.RecordingID = "11111111-1111-4111-8111-111111111111"
	p.State = "ready"
	p.FinalPrefix = "recordings/packages/" + p.ID + "/final/validated/"
	p.Inventory = assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1"}
	objects.bytes = map[string][]byte{}
	declarations := make([][]assets.RecordingPackageObject, 24)
	for _, r := range assets.LiveRenditions() {
		dir := filepath.Join(root, "raw", r.Name)
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		filter := fmt.Sprintf("scale=%d:%d,setsar=1,", r.Width, r.Height) + "drawbox=x=0:y=0:w=iw:h=ih/8:color=yellow:t=fill:enable='lt(t,240.24)',drawbox=x=0:y=0:w=iw:h=ih/8:color=green:t=fill:enable='gte(t,240.24)*lt(t,600.60)',drawbox=x=0:y=0:w=iw:h=ih/8:color=red:t=fill:enable='gte(t,600.60)'"
		args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=30000/1001:decimals=2", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "720.72", "-filter_threads", "2", "-vf", filter, "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "900", "-sc_threshold", "0", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
		reuse := false
		if os.Getenv("HHC_BROADCAST_FIXTURE_DIR") != "" {
			if _, err := os.Stat(filepath.Join(dir, "seg-000023.m4s")); err == nil {
				sar, e := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=sample_aspect_ratio", "-of", "default=nw=1:nk=1", filepath.Join(dir, "init.mp4")).Output()
				reuse = e == nil && strings.TrimSpace(string(sar)) == "1:1"
			}
		}
		if !reuse {
			if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("29.97 fixture %s: %v %s", r.Name, err, out)
			}
		}
		for seq := 0; seq < 24; seq++ {
			for _, name := range []string{"init.mp4", fmt.Sprintf("seg-%06d.m4s", seq)} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				path := r.Name + "/" + name
				objects.bytes[p.StagingKey(path)] = data
				objects.bytes[p.FinalPrefix+path] = data
				declarations[seq] = append(declarations[seq], assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
			}
		}
	}
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: t.TempDir()}
	var history []assets.RecordingLiveSegment
	for seq, inventory := range declarations {
		batch, err := probe.ValidateLiveSegment(ctx, p.ID, fmt.Sprintf("validated-%d", seq), seq, inventory)
		if err != nil {
			t.Fatalf("raw segment %d: %v", seq, err)
		}
		history, err = assets.AppendLiveSegment(history, batch, seq == 23)
		if err != nil {
			t.Fatalf("raw continuity %d: %v", seq, err)
		}
	}
	start, end := 8, 20
	policy := assets.RecordingBroadcastRange{MemberState: "vod", RecordingID: p.RecordingID, Epoch: 1, RangeRevision: 3, StartSequence: &start, EndSequenceExclusive: &end}
	projection := assets.BroadcastProjection(policy, history, 25, true)
	if projection.State != "ready" || projection.DurationSeconds == nil || math.Abs(*projection.DurationSeconds-360.36) > 1e-5 || projection.MediaStartSeconds == nil || *projection.MediaStartSeconds < 240 {
		t.Fatalf("decoded range: %+v", projection)
	}
	playlists, err := assets.RecordingLivePlaylists(history, true)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range playlists {
		objects.bytes[p.StagingKey(path)] = data
		objects.bytes[p.FinalPrefix+path] = data
	}
	for _, r := range assets.LiveRenditions() {
		r.FrameRate = 30000.0 / 1001
		r.DurationSeconds = history[23].Renditions[r.Name].End - history[0].Renditions[r.Name].Start
		r.SegmentCount = 24
		p.Inventory.Renditions = append(p.Inventory.Renditions, r)
	}
	for key, data := range objects.bytes {
		if strings.HasPrefix(key, p.StagingKey("")) {
			p.Inventory.Objects = append(p.Inventory.Objects, assets.RecordingPackageObject{Path: strings.TrimPrefix(key, p.StagingKey("")), SizeBytes: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
		}
	}
	before, err := assets.RecordingInventoryDigest(p.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	writes := map[string][]byte{}
	put := func(_ context.Context, key string, data []byte) error { writes[key] = data; return nil }
	if err = probe.GenerateCoversWithTimeline(ctx, p, "broadcast-covers", nil, history[start:end], put); err != nil {
		t.Fatal(err)
	}
	for key, data := range writes {
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := img.At(img.Bounds().Dx()/2, 10).RGBA()
		if g < 15000 || r > 8000 || b > 8000 {
			t.Fatalf("outside-range cover %s pixel=%d/%d/%d", key, r, g, b)
		}
	}
	if err = probe.GeneratePreviewsWithTimeline(ctx, p, "broadcast-previews", history, put); err != nil {
		t.Fatal(err)
	}
	vtt := string(writes[p.FinalPrefix+"previews/broadcast-previews/index.vtt"])
	if !strings.Contains(vtt, "00:04:00.240 --> 00:04:05.240\nseg-000008.jpg") {
		t.Fatalf("measured preview timeline missing: %s", vtt)
	}
	img, err := jpeg.Decode(bytes.NewReader(writes[p.FinalPrefix+"previews/broadcast-previews/seg-000008.jpg"]))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(80, 4).RGBA()
	if g < 15000 || r > 8000 || b > 8000 {
		t.Fatalf("outside-range first sprite pixel=%d/%d/%d", r, g, b)
	}
	after, err := assets.RecordingInventoryDigest(p.Inventory)
	if err != nil || after != before {
		t.Fatal("B1 modified raw inventory/digest", err)
	}
	if os.Getenv("HHC_BROADCAST_FIXTURE_DIR") != "" {
		rawPrefix := "recordings/captures/" + p.ID + "/final/"
		for key, data := range objects.bytes {
			if strings.HasPrefix(key, p.FinalPrefix) {
				writes[key] = data
			}
		}
		for _, d := range declarations {
			for _, o := range d {
				writes[rawPrefix+o.Path] = objects.bytes[p.StagingKey(o.Path)]
			}
		}
		for path, data := range playlists {
			writes[rawPrefix+"playlists/25/"+path] = data
		}
		writes[rawPrefix+"current.json"] = []byte(`{"revision":25,"lastSequence":23}`)
		writes[rawPrefix+"broadcast.json"], _ = json.Marshal(policy)
		writes[p.FinalPrefix+"previews/current.json"] = []byte(`{"attempt":"broadcast-previews"}`)
		writes["projection.json"], _ = json.Marshal(projection)
		writes["inventory.json"], _ = json.Marshal(p.Inventory)
		for key, data := range writes {
			name := filepath.Join(root, key)
			if err = os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(name, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("verified fixture: %s origin=%.6f duration=%.6f rawDigest=%s", root, *projection.MediaStartSeconds, *projection.DurationSeconds, before)
	}
}
