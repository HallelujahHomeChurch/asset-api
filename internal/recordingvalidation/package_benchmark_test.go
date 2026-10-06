package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"hhc/asset-api/internal/assets"
)

// Protects parallel fragment validation from corrupting the storage fixture.
func TestPackageObjectsConcurrentFixture(t *testing.T) {
	_, objects := packageTransferFixture()
	ctx := context.Background()
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			key := fmt.Sprintf("final/segment-%d", i)
			for j := 0; j < 20; j++ {
				if err := objects.CopyPackageObject(ctx, "recordings/packages/package-a/staging/720p/seg-000000.m4s", key, "etag"); err != nil {
					t.Error(err)
					return
				}
				body, err := objects.Open(ctx, key)
				if err != nil {
					t.Error(err)
					return
				}
				data, readErr := io.ReadAll(body)
				closeErr := body.Close()
				if readErr != nil || closeErr != nil || string(data) != "segment" {
					t.Errorf("copied bytes = %q; read=%v close=%v", data, readErr, closeErr)
					return
				}
			}
		}(i)
	}
	workers.Wait()
	if objects.copies != 160 || objects.opens != 160 || objects.openedBytes != 1120 {
		t.Fatalf("operations: copies=%d opens=%d bytes=%d", objects.copies, objects.opens, objects.openedBytes)
	}
}

func realPackageFixture(t testing.TB) (assets.RecordingPackage, *packageMemoryObjects, PackageMediaProbe) {
	return realPackageFixtureAtFrameRate(t, 30)
}

func realPackageFixtureAtFrameRate(t testing.TB, frameRate int) (assets.RecordingPackage, *packageMemoryObjects, PackageMediaProbe) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=1280x720:rate=%d", frameRate), "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "35", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-profile:v", "baseline", "-level:v", "3.1", "-pix_fmt", "yuv420p", "-g", fmt.Sprint(30 * frameRate), "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "seg-%06d.m4s"), filepath.Join(dir, "index.m3u8")}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v %s", err, out)
	}
	p, objects := packageTransferFixture()
	p.Inventory.Renditions[0].DurationSeconds = 35
	p.Inventory.Renditions[0].SegmentCount = 2
	p.Inventory.Renditions[0].FrameRate = float64(frameRate)
	p.Inventory.Objects = append(p.Inventory.Objects, assets.RecordingPackageObject{Path: "720p/seg-000001.m4s"})
	objects.bytes[p.StagingKey("master.m3u8")] = []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=12,AVERAGE-BANDWIDTH=12,RESOLUTION=1280x720,CODECS=\"avc1.42c01f,mp4a.40.2\"\n720p/index.m3u8\n")
	for i, o := range p.Inventory.Objects {
		if o.Path != "master.m3u8" {
			data, err := os.ReadFile(filepath.Join(dir, filepath.Base(o.Path)))
			if err != nil {
				t.Fatal(err)
			}
			objects.bytes[p.StagingKey(o.Path)] = data
		}
		data := objects.bytes[p.StagingKey(o.Path)]
		hash := sha256.Sum256(data)
		p.Inventory.Objects[i].SizeBytes = int64(len(data))
		p.Inventory.Objects[i].SHA256 = hex.EncodeToString(hash[:])
	}
	peak, average, err := assets.RecordingPlaylistBitrates([]int64{int64(len(objects.bytes[p.StagingKey("720p/seg-000000.m4s")])), int64(len(objects.bytes[p.StagingKey("720p/seg-000001.m4s")]))}, []float64{30, 5}, 30)
	if err != nil {
		t.Fatal(err)
	}
	master := []byte(fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=1280x720,CODECS=\"avc1.42c01f,mp4a.40.2\"\n720p/index.m3u8\n", peak, average))
	objects.bytes[p.StagingKey("master.m3u8")] = master
	for i, o := range p.Inventory.Objects {
		if o.Path == "master.m3u8" {
			hash := sha256.Sum256(master)
			p.Inventory.Objects[i].SizeBytes = int64(len(master))
			p.Inventory.Objects[i].SHA256 = hex.EncodeToString(hash[:])
		}
	}
	p.Inventory.InventoryDigest, err = assets.RecordingInventoryDigest(p.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	p.SizeBytes, err = assets.ValidateRecordingInventory(p.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	return p, objects, PackageMediaProbe{Objects: objects, FFmpeg: ffmpeg, FFprobe: ffprobe, ScratchRoot: t.TempDir()}
}

// The same fixture and tool build are used before and after the pipeline change.
func BenchmarkPackageValidation(b *testing.B) {
	p, objects, probe := realPackageFixture(b)
	b.ReportAllocs()
	b.SetBytes(p.SizeBytes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := FreezeRecordingPackage(context.Background(), p, fmt.Sprintf("bench-%d", i), objects, probe); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(objects.heads)/float64(b.N), "HEAD/op")
	b.ReportMetric(float64(objects.copies)/float64(b.N), "COPY/op")
	b.ReportMetric(float64(objects.opens)/float64(b.N), "GET/op")
	b.ReportMetric(float64(objects.openedBytes)/float64(b.N), "GET-bytes/op")
	b.ReportMetric(float64(objects.puts)/float64(b.N), "inventory-PUT/op")
}
