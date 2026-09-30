package recordingvalidation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hhc/asset-api/internal/assets"
)

func TestPackageProbeDecodesRealFragment(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "35", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "900", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %s %v", out, err)
	}
	p, objects := packageTransferFixture()
	p.Inventory.Renditions[0].DurationSeconds = 35
	p.Inventory.Renditions[0].SegmentCount = 2
	p.Inventory.Objects = append(p.Inventory.Objects, assets.RecordingPackageObject{Path: "720p/seg-000001.m4s"})
	for i, o := range p.Inventory.Objects {
		if strings.HasSuffix(o.Path, ".mp4") || strings.HasSuffix(o.Path, ".m4s") {
			b, err := os.ReadFile(filepath.Join(dir, filepath.Base(o.Path)))
			if err != nil {
				t.Fatal(err)
			}
			objects.bytes["final/"+o.Path] = b
			p.Inventory.Objects[i].SizeBytes = int64(len(b))
		}
	}
	objects.bytes["final/master.m3u8"] = []byte(strings.Replace(string(objects.bytes[p.StagingKey("master.m3u8")]), "avc1.64001f", "avc1.42c01f", 1))
	probe := PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: t.TempDir()}
	if err := probe.Validate(context.Background(), p.Inventory, "final/"); err != nil {
		for _, track := range []string{"v:0", "a:0"} {
			out, _ := exec.Command(ffprobe, "-v", "error", "-protocol_whitelist", "file,concat", "-select_streams", track, "-read_intervals", "%+#1", "-show_packets", "-show_entries", "packet=stream_index,pts_time,duration_time,flags", "-of", "json", "concat:"+filepath.Join(dir, "init.mp4")+"|"+filepath.Join(dir, "seg-000000.m4s")).Output()
			t.Logf("generated fixture first %s packet: %s", track, out)
		}
		t.Fatalf("real probe: %v", err)
	}
	entries, err := os.ReadDir(probe.ScratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch retained: %v %v", entries, err)
	}
	objects.bytes["final/720p/seg-000000.m4s"] = []byte("invalid")
	if err := probe.Validate(context.Background(), p.Inventory, "final/"); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("invalid fragment: %v", err)
	}
}

const segmentProbeFixture = `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1280,"height":720,"pix_fmt":"yuv420p","sample_aspect_ratio":"1:1","r_frame_rate":"30/1","extradata":"\n00000000: 0164 001f ffe1 001a 6764 001f acd9 4050  .d......gd....@P\n"},{"index":1,"codec_name":"aac","codec_type":"audio","profile":"LC","sample_rate":"48000","channels":2}],"packets":[{"stream_index":0,"pts_time":"0.000000","duration_time":"0.033333","flags":"K_"},{"stream_index":1,"pts_time":"0.000000","duration_time":"0.021333","flags":"K_"},{"stream_index":0,"pts_time":"0.033333","duration_time":"0.033333","flags":"__"},{"stream_index":1,"pts_time":"0.021333","duration_time":"0.021333","flags":"K_"},{"stream_index":1,"pts_time":"0.042666","duration_time":"0.021333","flags":"K_"}]}`

func TestFragmentProbeChecksCodecDimensionsIDRAndTimeline(t *testing.T) {
	r := assets.RecordingRendition{Width: 1280, Height: 720, FrameRate: 30}
	got, err := validatePackageSegmentProbe([]byte(segmentProbeFixture), r)
	if err != nil || got.Codecs != "avc1.64001f,mp4a.40.2" || got.Start != 0 || got.End < 0.066665 || got.End > 0.066668 {
		t.Fatalf("probe: %+v %v", got, err)
	}
	for _, tc := range []struct{ old, replacement string }{
		{`"codec_name":"h264"`, `"codec_name":"hevc"`},
		{`"width":1280`, `"width":1920`},
		{`"pix_fmt":"yuv420p"`, `"pix_fmt":"yuv420p10le"`},
		{`"sample_aspect_ratio":"1:1"`, `"sample_aspect_ratio":"2:1"`},
		{`"channels":2`, `"channels":6`},
		{`"profile":"LC"`, `"profile":"HE-AAC"`},
		{`"flags":"K_"`, `"flags":"__"`},
		{`"pts_time":"0.000000"`, `"pts_time":"NaN"`},
		{`"pts_time":"0.033333"`, `"pts_time":"0.000000"`},
		{`"r_frame_rate":"30/1"`, `"r_frame_rate":"60/1"`},
	} {
		if _, err := validatePackageSegmentProbe([]byte(strings.Replace(segmentProbeFixture, tc.old, tc.replacement, 1)), r); !errors.Is(err, assets.ErrInvalidUpload) {
			t.Fatalf("invalid probe accepted %s: %v", tc.old, err)
		}
	}
}

func TestFirstPacketRequiresActualIDRNAL(t *testing.T) {
	valid := []byte(`{"packets":[{"flags":"K_","data":"\n00000000: 0000 0002 0600 0000 0002 6500            ..........e.\n"}]}`)
	if err := validateFirstIDRPacket(valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(valid), "6500", "4100", 1), strings.Replace(string(valid), "0002 6500", "0003 6500", 1), `{"packets":[]}`} {
		if err := validateFirstIDRPacket([]byte(bad)); !errors.Is(err, assets.ErrInvalidUpload) {
			t.Fatalf("non-IDR accepted: %v", err)
		}
	}
}

func TestFragmentProbeInfersOnlyMissingFirstAACDurationFromNextPTS(t *testing.T) {
	r := assets.RecordingRendition{Width: 1280, Height: 720, FrameRate: 30}
	data := strings.Replace(segmentProbeFixture, `"stream_index":1,"pts_time":"0.000000","duration_time":"0.021333"`, `"stream_index":1,"pts_time":"0.000000"`, 1)
	if _, err := validatePackageSegmentProbe([]byte(data), r); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(data, `"stream_index":1,"pts_time":"0.021333","duration_time":"0.021333"`, `"stream_index":1,"pts_time":"0.021333"`, 1)
	if _, err := validatePackageSegmentProbe([]byte(bad), r); !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("missing later duration accepted: %v", err)
	}
}
