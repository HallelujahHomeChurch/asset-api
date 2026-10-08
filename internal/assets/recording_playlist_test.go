package assets

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func fractionalFrameRatePackageFixture(t *testing.T, duration float64) (RecordingPackageInventory, map[string][]byte) {
	t.Helper()
	inv := captureInventoryFixture()
	inv.InventoryDigest = ""
	inv.Objects = inv.Objects[:1]
	files := map[string][]byte{}
	master := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n"
	for i := range inv.Renditions {
		r := &inv.Renditions[i]
		r.FrameRate, r.DurationSeconds, r.SegmentCount = 30000.0/1001, duration, 8
		media := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:30\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4\"\n"
		for _, name := range []string{"index.m3u8", "init.mp4"} {
			inv.Objects = append(inv.Objects, RecordingPackageObject{Path: r.Name + "/" + name, SizeBytes: 100, SHA256: strings.Repeat("a", 64)})
		}
		var sizes []int64
		var durations []float64
		for seq := 0; seq < 8; seq++ {
			seconds := 30.03
			if seq == 7 {
				seconds = duration - 7*30.03
			}
			name := fmt.Sprintf("seg-%06d.m4s", seq)
			media += fmt.Sprintf("#EXTINF:%.6f,\n%s\n", seconds, name)
			inv.Objects = append(inv.Objects, RecordingPackageObject{Path: r.Name + "/" + name, SizeBytes: 100, SHA256: strings.Repeat("a", 64)})
			sizes, durations = append(sizes, 100), append(durations, seconds)
		}
		files[r.Name+"/index.m3u8"] = []byte(media + "#EXT-X-ENDLIST\n")
		peak, average, err := RecordingPlaylistBitrates(sizes, durations, 30)
		if err != nil {
			t.Fatal(err)
		}
		master += fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"avc1.640028,mp4a.40.2\",FRAME-RATE=29.970\n%s/index.m3u8\n", peak, average, r.Width, r.Height, r.Name)
	}
	files["master.m3u8"] = []byte(master)
	inv.InventoryDigest, _ = RecordingInventoryDigest(inv)
	return inv, files
}

func TestRecordingPlaylistFractionalFrameRateUsesActualSegmentCount(t *testing.T) {
	for _, duration := range []float64{240.206633, 215.21} {
		inv, files := fractionalFrameRatePackageFixture(t, duration)
		if err := ValidateRecordingPlaylists(inv, files); err != nil {
			t.Fatalf("8 real segments, duration %.6f: %v", duration, err)
		}
	}
	for _, replacement := range []string{"#EXTINF:5.000000,", "#EXTINF:30.100000,"} {
		inv, files := fractionalFrameRatePackageFixture(t, 240.206633)
		files["720p/index.m3u8"] = []byte(strings.Replace(string(files["720p/index.m3u8"]), "#EXTINF:30.030000,", replacement, 1))
		if err := ValidateRecordingPlaylists(inv, files); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("invalid interior segment accepted: %v", err)
		}
	}
}

func playlistFixture() (RecordingPackageInventory, map[string][]byte) {
	inv := packageFixture()
	inv.Renditions[0].DurationSeconds = 35
	inv.Renditions[0].SegmentCount = 2
	inv.Objects = append(inv.Objects, RecordingPackageObject{Path: "720p/seg-000001.m4s", SizeBytes: 100, SHA256: strings.Repeat("e", 64)})
	high := inv.Renditions[0]
	high.Name = "1080p"
	high.Width = 1920
	high.Height = 1080
	high.VideoBitrate = 3000000
	inv.Renditions = append(inv.Renditions, high)
	for _, o := range inv.Objects[:len(inv.Objects)] {
		if strings.HasPrefix(o.Path, "720p/") {
			o.Path = strings.Replace(o.Path, "720p/", "1080p/", 1)
			inv.Objects = append(inv.Objects, o)
		}
	}
	master := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=46,AVERAGE-BANDWIDTH=46,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\n720p/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=46,AVERAGE-BANDWIDTH=46,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\"\n1080p/index.m3u8\n"
	media := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:30\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:30.000000,\nseg-000000.m4s\n#EXTINF:5.000000,\nseg-000001.m4s\n#EXT-X-ENDLIST\n"
	return inv, map[string][]byte{"master.m3u8": []byte(master), "720p/index.m3u8": []byte(media), "1080p/index.m3u8": []byte(media)}
}

func TestRecordingPlaylistClosureAndAlignedBoundaries(t *testing.T) {
	inv, files := playlistFixture()
	if err := ValidateRecordingPlaylists(inv, files); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, old, replacement string }{
		{"external master", "master.m3u8", "720p/index.m3u8", "https://evil/720p/index.m3u8"},
		{"master resolution", "master.m3u8", "1280x720", "1920x1080"},
		{"master codec", "master.m3u8", "avc1.64001f", "hev1.1.6.L93"},
		{"key URL", "720p/index.m3u8", "#EXT-X-MAP:", "#EXT-X-KEY:METHOD=AES-128,URI=\"https://evil/key\"\n#EXT-X-MAP:"},
		{"external map", "720p/index.m3u8", "URI=\"init.mp4\"", "URI=\"https://evil/init.mp4\""},
		{"encoded path", "720p/index.m3u8", "seg-000000.m4s", "%2e%2e/seg-000000.m4s"},
		{"missing segment", "720p/index.m3u8", "#EXTINF:5.000000,\nseg-000001.m4s\n", ""},
		{"missing end", "720p/index.m3u8", "#EXT-X-ENDLIST\n", ""},
		{"unbounded duration", "720p/index.m3u8", "30.000000", "Inf"},
		{"unaligned", "1080p/index.m3u8", "30.000000", "29.500000"},
		{"duplicate header", "720p/index.m3u8", "#EXT-X-TARGETDURATION:30", "#EXT-X-TARGETDURATION:30\n#EXT-X-TARGETDURATION:30"},
		{"range indirection", "720p/index.m3u8", "#EXTINF:5.000000,", "#EXT-X-BYTERANGE:10@1\n#EXTINF:5.000000,"},
		{"extra object", "720p/index.m3u8", "#EXT-X-ENDLIST", "secret.mp4\n#EXT-X-ENDLIST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, files := playlistFixture()
			files[tc.path] = []byte(strings.Replace(string(files[tc.path]), tc.old, tc.replacement, 1))
			if err := ValidateRecordingPlaylists(inv, files); !errors.Is(err, ErrInvalidUpload) {
				t.Fatalf("unsafe playlist accepted: %v", err)
			}
		})
	}
}

func TestRecordingMasterRejectsInventedBandwidth(t *testing.T) {
	inv, files := playlistFixture()
	// Each rendition has two 100-byte segments lasting 30s and 5s. The
	// eligible peak window is both segments: ceil(1600/35)=46 bit/s.
	if err := ValidateRecordingPlaylists(inv, files); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{"BANDWIDTH=1,AVERAGE-BANDWIDTH=46", "BANDWIDTH=46,AVERAGE-BANDWIDTH=1", "BANDWIDTH=160,AVERAGE-BANDWIDTH=46"} {
		bad := make(map[string][]byte, len(files))
		for k, v := range files {
			bad[k] = v
		}
		bad["master.m3u8"] = []byte(strings.Replace(string(files["master.m3u8"]), "BANDWIDTH=46,AVERAGE-BANDWIDTH=46", replacement, 1))
		if err := ValidateRecordingPlaylists(inv, bad); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("accepted invented throughput %s: %v", replacement, err)
		}
	}
}

func TestRecordingPlaylistBitratesUseActualEligibleWindows(t *testing.T) {
	for _, tc := range []struct {
		sizes         []int64
		durations     []float64
		target        int
		peak, average int64
	}{
		{[]int64{100, 100}, []float64{30, 5}, 30, 46, 46},
		{[]int64{3750000, 7500000, 625000}, []float64{30, 30, 5}, 30, 2000000, 1461539},
		{[]int64{7}, []float64{5}, 5, 12, 12},
	} {
		peak, average, err := RecordingPlaylistBitrates(tc.sizes, tc.durations, tc.target)
		if err != nil || peak != tc.peak || average != tc.average {
			t.Fatalf("rates %d/%d %v", peak, average, err)
		}
	}
	for _, durations := range [][]float64{{30, 0}, {5, 5}, {30, 32}, {30, math.NaN()}} {
		if _, _, err := RecordingPlaylistBitrates([]int64{100, 100}, durations, 30); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("accepted invalid durations %v", durations)
		}
	}
}
