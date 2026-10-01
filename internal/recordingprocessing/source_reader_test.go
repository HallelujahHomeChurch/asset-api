package recordingprocessing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type rangeObjects struct {
	mu      sync.Mutex
	data    []byte
	ranges  []assets.ByteRange
	badETag bool
	keys    []string
}

func TestTailMoovSourceUsesSeekableLoopbackReader(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "tail-moov.mp4")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-t", "3", "-c:v", "libx264", "-preset", "ultrafast", file).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Index(data, []byte("moov")) < bytes.Index(data, []byte("mdat")) {
		t.Fatal("fixture must keep moov at tail")
	}
	objects := &rangeObjects{data: data}
	key := "recording-sources/" + strings.Repeat("a", 32) + "/final/" + strings.Repeat("b", 32) + "/source"
	reader, err := NewSourceReader(ctx, objects, key, int64(len(data)), "verified-etag")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-protocol_whitelist", "http,tcp", "-format_whitelist", "mov", "-show_entries", "format=duration", "-of", "csv=p=0", reader.URL()).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "3.000000" {
		t.Fatalf("tail probe: %v %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-nostdin", "-protocol_whitelist", "http,tcp", "-format_whitelist", "mov", "-ss", "2", "-i", reader.URL(), "-frames:v", "1", "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("tail seek/decode: %v %s", err, out)
	}
}

func (o *rangeObjects) Open(_ context.Context, key string, r assets.ByteRange, etag string) (assets.BlobDownload, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if etag != "verified-etag" {
		return assets.BlobDownload{}, assets.ErrConflict
	}
	o.keys = append(o.keys, key)
	o.ranges = append(o.ranges, r)
	if r.Count <= 0 || r.Count > sourceReadWindow || r.Offset < 0 || r.Offset+r.Count > int64(len(o.data)) {
		return assets.BlobDownload{}, assets.ErrInvalidInput
	}
	if o.badETag {
		etag = "changed"
	}
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader(o.data[r.Offset : r.Offset+r.Count])), Size: r.Count, TotalSize: int64(len(o.data)), ETag: etag}, nil
}

func TestSourceReaderFixedObjectTailRangeAndBoundedStreaming(t *testing.T) {
	key := "recording-sources/" + strings.Repeat("a", 32) + "/final/" + strings.Repeat("b", 32) + "/source"
	objects := &rangeObjects{data: bytes.Repeat([]byte("0123456789"), 2_000_000)}
	reader, err := NewSourceReader(context.Background(), objects, key, int64(len(objects.data)), "verified-etag")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, tc := range []struct {
		method, header string
		status         int
		start, end     int64
	}{
		{"HEAD", "", 200, 0, 0}, {"GET", "bytes=-17", 206, int64(len(objects.data)) - 17, int64(len(objects.data))},
		{"GET", "bytes=7-13", 206, 7, 14}, {"GET", "", 200, 0, int64(len(objects.data))},
		{"GET", "bytes=999999999-", 416, 0, 0},
	} {
		r, _ := http.NewRequest(tc.method, reader.URL(), nil)
		if tc.header != "" {
			r.Header.Set("Range", tc.header)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != tc.status {
			t.Fatalf("%+v: status %d error %v", tc, response.StatusCode, readErr)
		}
		if tc.status < 400 && !bytes.Equal(body, objects.data[tc.start:tc.end]) {
			t.Fatalf("wrong response %+v", tc)
		}
	}
	objects.mu.Lock()
	defer objects.mu.Unlock()
	if len(objects.ranges) < 3 {
		t.Fatal("did not exercise SDK range refill")
	}
	if objects.ranges[0].Offset != int64(len(objects.data))-17 {
		t.Fatal("HEAD read source or suffix started at wrong offset")
	}
	for i, r := range objects.ranges {
		if r.Count > sourceReadWindow || objects.keys[i] != key {
			t.Fatalf("unbounded/arbitrary source read: %+v", r)
		}
	}
}

func TestSourceReaderRejectsDifferentObjectAndStaleETag(t *testing.T) {
	key := "recording-sources/" + strings.Repeat("a", 32) + "/final/" + strings.Repeat("b", 32) + "/source"
	objects := &rangeObjects{data: []byte("media"), badETag: true}
	for _, bad := range []string{"https://external.invalid/source", "recording-sources/id/staging", key + "/../source"} {
		if reader, err := NewSourceReader(context.Background(), objects, bad, 5, "etag"); err == nil {
			reader.Close()
			t.Fatal("accepted untrusted source key")
		}
	}
	reader, err := NewSourceReader(context.Background(), objects, key, 5, "verified-etag")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, suffix := range []string{"?url=https://external.invalid", "/../source"} {
		response, err := http.Get(reader.URL() + suffix)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatalf("arbitrary source selection %d", response.StatusCode)
		}
	}
	response, err := http.Get(reader.URL())
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if bytes.Equal(body, objects.data) || readErr == nil && response.StatusCode < 400 {
		t.Fatalf("stale source served: %d %s %v", response.StatusCode, body, readErr)
	}
	if strings.Contains(fmt.Sprint(readErr), key) {
		t.Fatal("storage path leaked")
	}
}
