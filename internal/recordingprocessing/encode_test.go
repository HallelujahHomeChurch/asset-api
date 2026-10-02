package recordingprocessing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/recordingvalidation"
)

func TestBrowserEncodeReadsRangesAndSpoolsThirtySecondHLS(t *testing.T) {
	ffmpeg, e1 := exec.LookPath("ffmpeg")
	ffprobe, e2 := exec.LookPath("ffprobe")
	if e1 != nil || e2 != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal("media fixtures missing")
		}
		t.Skip("media fixtures missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path := filepath.Join(t.TempDir(), "source.mp4")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=s=1920x1080:r=30", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "35", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", path).CombinedOutput(); err != nil {
		t.Fatalf("source fixture %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := "recording-sources/" + strings.Repeat("a", 32) + "/final/" + strings.Repeat("b", 32) + "/source"
	source, err := NewSourceReader(ctx, &rangeObjects{data: data}, key, int64(len(data)), "verified-etag")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	storage := &spoolObjects{}
	spool, err := NewOutputSpool(ctx, storage, strings.Repeat("c", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	plan, err := EncodeSource(ctx, source, spool, ffmpeg, ffprobe)
	if err != nil || len(plan.Renditions) != 2 {
		t.Fatalf("encode plan %+v %v", plan, err)
	}
	objects, lists, err := spool.Snapshot()
	if err != nil || len(objects) != 6 || len(lists) != 2 {
		t.Fatalf("encoded closure %d %d %v", len(objects), len(lists), err)
	}
	for _, name := range []string{"720p", "1080p"} {
		if !strings.Contains(string(lists[name+"/index.m3u8"]), "#EXTINF:30.000000,") || !strings.Contains(string(lists[name+"/index.m3u8"]), "#EXT-X-ENDLIST") {
			t.Fatalf("missing 30-second VOD %s", name)
		}
	}
	probe := recordingvalidation.PackageMediaProbe{Objects: storage, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: t.TempDir()}
	inv, err := BuildSourcePackage(ctx, plan, spool, probe)
	if err != nil {
		t.Fatal(err)
	}
	size, err := assets.ValidateRecordingInventory(inv)
	if err != nil {
		t.Fatal(err)
	}
	pkg := assets.RecordingPackage{ID: strings.Repeat("c", 32), Inventory: inv, SizeBytes: size}
	if _, err := recordingvalidation.FreezeRecordingPackage(ctx, pkg, pkg.ID, storage, probe.Validate); err != nil {
		t.Fatalf("encoded package failed independent full validation: %v", err)
	}
	t.Run("fenced source orchestration", func(t *testing.T) {
		sources := &finalizeObjects{rangeObjects: rangeObjects{data: data}, state: "success"}
		outputs := &spoolObjects{}
		claim := postgres.RecordingSourceClaim{ClaimID: strings.Repeat("d", 32), Source: assets.RecordingSource{ID: strings.Repeat("a", 32), CopyAttemptID: strings.Repeat("b", 32), SizeBytes: int64(len(data)), ChecksumSHA256: fmt.Sprintf("%x", sha256.Sum256(data))}}
		probe.ScratchRoot = t.TempDir()
		if _, err := processSourceClaim(ctx, claim, func(context.Context, assets.RecordingSourceCopy) error { return assets.ErrConflict }, sources, outputs, probe); !errors.Is(err, assets.ErrConflict) || len(outputs.writes) != 0 {
			t.Fatalf("stale checkpoint encoded output: %v", err)
		}
		checkpointed := false
		inv, err := processSourceClaim(ctx, claim, func(_ context.Context, copy assets.RecordingSourceCopy) error { checkpointed = true; return nil }, sources, outputs, probe)
		if err != nil || !checkpointed || len(inv.Renditions) != 2 {
			t.Fatalf("source pipeline: %v checkpoint=%t", err, checkpointed)
		}
		entries, err := os.ReadDir(probe.ScratchRoot)
		if err != nil || len(entries) != 0 {
			t.Fatalf("pipeline retained scratch: %v %v", entries, err)
		}
		if _, _, err := outputs.Head(ctx, "recordings/packages/"+claim.ClaimID+"/final/"+claim.ClaimID+"/package.json"); err != nil {
			t.Fatal(err)
		}
	})
}
