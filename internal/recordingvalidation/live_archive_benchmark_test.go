package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

// Measures long-inventory provider fan-out only. Media receipts are synthetic;
// real-media correctness is tested separately, and this is not a live SLO.
func BenchmarkLongLiveArchiveCopies(b *testing.B) {
	for _, segments := range []int{900, 990, 1078} {
		b.Run(fmt.Sprint(segments), func(b *testing.B) {
			p, history, objects := longArchiveMetadataFixture(b, segments)
			provider := latencyLiveObjects{objects, 250 * time.Millisecond}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, used, err := freezeLiveArchive(context.Background(), p, fmt.Sprintf("archive-%d", i), history, provider, PackageMediaProbe{})
				if err != nil || !used {
					b.Fatalf("archive metadata: %v %v", used, err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(segments*3+3), "media_objects/op")
		})
	}
}
func longArchiveMetadataFixture(tb testing.TB, count int) (assets.RecordingPackage, []assets.RecordingLiveSegment, *packageMemoryObjects) {
	tb.Helper()
	p := assets.RecordingPackage{ID: strings.Repeat("a", 32), Inventory: assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: assets.RecordingTenSecondPreset}}
	history := make([]assets.RecordingLiveSegment, count)
	for i := range history {
		history[i] = assets.RecordingLiveSegment{Sequence: i, Renditions: make(map[string]assets.RecordingLiveFragment)}
	}
	master := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n"
	for _, r := range assets.LiveRenditions() {
		r.FrameRate = 30000.0 / 1001
		r.SegmentCount = count
		r.DurationSeconds = float64(count) * 10.01
		p.Inventory.Renditions = append(p.Inventory.Renditions, r)
		init := assets.RecordingPackageObject{Path: r.Name + "/init.mp4", SizeBytes: 128, SHA256: strings.Repeat("a", 64)}
		p.Inventory.Objects = append(p.Inventory.Objects, init)
		sizes, durations := make([]int64, count), make([]float64, count)
		for i := range count {
			size := int64(float64(r.VideoBitrate+r.AudioBitrate) * 10.01 / 8)
			segment := assets.RecordingPackageObject{Path: fmt.Sprintf("%s/seg-%06d.m4s", r.Name, i), SizeBytes: size, SHA256: strings.Repeat("b", 64)}
			p.Inventory.Objects = append(p.Inventory.Objects, segment)
			history[i].Renditions[r.Name] = assets.RecordingLiveFragment{Start: float64(i) * 10.01, End: float64(i+1) * 10.01, FrameRate: r.FrameRate, VideoFrames: 300, Codecs: "avc1.64001f,mp4a.40.2", Validation: &assets.RecordingLiveValidation{Version: liveValidationVersion, Init: init, Segment: segment, InitETag: "etag", SegmentETag: "etag"}}
			sizes[i], durations[i] = size, 10.01
		}
		peak, average, err := assets.RecordingPlaylistBitrates(sizes, durations, 11)
		if err != nil {
			tb.Fatal(err)
		}
		master += fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%.3f,CODECS=\"avc1.64001f,mp4a.40.2\"\n%s/index.m3u8\n", peak, average, r.Width, r.Height, r.FrameRate, r.Name)
	}
	playlists, err := assets.RecordingLivePlaylists(history, true, 300)
	if err != nil {
		tb.Fatal(err)
	}
	playlists["master.m3u8"] = []byte(master)
	objects := &packageMemoryObjects{bytes: make(map[string][]byte)}
	for name, data := range playlists {
		data = []byte(strings.Replace(string(data), "#EXT-X-PLAYLIST-TYPE:EVENT", "#EXT-X-PLAYLIST-TYPE:VOD", 1))
		hash := sha256.Sum256(data)
		p.Inventory.Objects = append(p.Inventory.Objects, assets.RecordingPackageObject{Path: name, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
		objects.bytes[p.StagingKey(name)] = data
	}
	p.Inventory.InventoryDigest, err = assets.RecordingInventoryDigest(p.Inventory)
	if err != nil {
		tb.Fatal(err)
	}
	p.SizeBytes, err = assets.ValidateRecordingInventory(p.Inventory)
	if err != nil {
		tb.Fatal(err)
	}
	return p, history, objects
}
