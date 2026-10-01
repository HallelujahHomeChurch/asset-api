package recordingprocessing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type spoolObjects struct {
	mu      sync.Mutex
	writes  map[string][]byte
	wait    <-chan struct{}
	started chan struct{}
	failure error
}

func (o *spoolObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.writes[key]
	if !ok {
		return nil, assets.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), nil
}

func (o *spoolObjects) Head(_ context.Context, key string) (int64, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.writes[key]
	if !ok {
		return 0, "", assets.ErrNotFound
	}
	return int64(len(data)), "etag", nil
}

func (o *spoolObjects) CopyPackageObject(_ context.Context, source, target, etag string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.writes[source]
	if !ok || etag != "etag" {
		return assets.ErrNotFound
	}
	if _, exists := o.writes[target]; exists {
		return assets.ErrConflict
	}
	o.writes[target] = bytes.Clone(data)
	return nil
}

func (o *spoolObjects) PutPackageInventory(ctx context.Context, key string, data []byte) error {
	return o.PutRecordingObject(ctx, key, bytes.NewReader(data), int64(len(data)), "application/json")
}

func (o *spoolObjects) PutRecordingObject(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error {
	if o.failure != nil {
		return o.failure
	}
	if o.started != nil {
		select {
		case o.started <- struct{}{}:
		default:
		}
	}
	if o.wait != nil {
		select {
		case <-o.wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.writes == nil {
		o.writes = map[string][]byte{}
	}
	o.writes[key] = data
	return nil
}

func TestSpoolBudgetIncludesAlreadyUploadedAndRemovedObjects(t *testing.T) {
	objects := &spoolObjects{}
	spool, err := NewOutputSpool(context.Background(), objects, strings.Repeat("a", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	spool.bytes = assets.RecordingPackageMaxBytes - assets.RecordingInventoryMaxBytes - 3*assets.RecordingPlaylistMaxBytes - 2
	r, _ := http.NewRequest("PUT", spool.URL()+"/720p/seg-000001.m4s", strings.NewReader("media"))
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 422 {
		t.Fatal("cumulative budget reset after deleting local spool")
	}
	if _, _, err := spool.Snapshot(); !errors.Is(err, assets.ErrRecordingPackageTooLarge) {
		t.Fatalf("wrong budget result: %v", err)
	}
	entries, err := os.ReadDir(spool.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed object retained: %v %v", entries, err)
	}
	if len(objects.writes) != 0 {
		t.Fatal("over-budget object reached R2")
	}
}

func TestSpoolStorageFailureIsStickyRedactedAndCleaned(t *testing.T) {
	objects := &spoolObjects{failure: errors.New("signed credential must not escape")}
	spool, err := NewOutputSpool(context.Background(), objects, strings.Repeat("a", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	for i := 0; i < 2; i++ {
		r, _ := http.NewRequest("PUT", spool.URL()+"/720p/seg-000000.m4s", strings.NewReader("media"))
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 422 || bytes.Contains(body, []byte("credential")) {
			t.Fatal("storage failure accepted/leaked")
		}
	}
	if _, _, err := spool.Snapshot(); !errors.Is(err, ErrOutputUpload) {
		t.Fatalf("upload result: %v", err)
	}
	entries, err := os.ReadDir(spool.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed object retained: %v %v", entries, err)
	}
}

func TestSpoolBackpressureAndCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resume := make(chan struct{})
	objects := &spoolObjects{wait: resume, started: make(chan struct{}, 1)}
	root := t.TempDir()
	spool, err := NewOutputSpool(ctx, objects, strings.Repeat("a", 32), root)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	done := make(chan error, 1)
	go func() {
		r, _ := http.NewRequestWithContext(ctx, "PUT", spool.URL()+"/720p/seg-000000.m4s", strings.NewReader("media"))
		response, err := http.DefaultClient.Do(r)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 200 {
				err = io.ErrUnexpectedEOF
			}
		}
		done <- err
	}()
	select {
	case <-objects.started:
	case <-ctx.Done():
		t.Fatal("upload did not reach storage")
	}
	select {
	case err := <-done:
		t.Fatalf("PUT acknowledged before R2: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	items, playlists, err := spool.Snapshot()
	if err != nil || len(items) != 1 || items[0].SizeBytes != 5 || len(playlists) != 0 {
		t.Fatalf("spool snapshot %v %v", items, err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch retained: %v %v", entries, err)
	}
}

func TestFFmpegFMP4HTTPSpoolProducesCompleteObjects(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("HHC_REQUIRE_MEDIA_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip("ffmpeg unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	objects := &spoolObjects{}
	spool, err := NewOutputSpool(ctx, objects, strings.Repeat("b", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	base := spool.URL() + "/720p/"
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=s=1280x720:r=2", "-t", "31", "-c:v", "libx264", "-preset", "ultrafast", "-g", "60", "-sc_threshold", "0", "-f", "hls", "-hls_time", "30", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-method", "PUT", "-hls_segment_filename", base + "seg-%06d.m4s", base + "index.m3u8"}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("native HTTP fMP4: %v %s", err, out)
	}
	items, playlists, err := spool.Snapshot()
	if err != nil || len(items) != 3 || !bytes.Contains(playlists["720p/index.m3u8"], []byte("#EXT-X-ENDLIST")) {
		t.Fatalf("incomplete mux output objects=%d lists=%v err=%v", len(items), playlists, err)
	}
	objects.mu.Lock()
	defer objects.mu.Unlock()
	if len(objects.writes) != 3 {
		t.Fatalf("not all init/segments persisted: %d", len(objects.writes))
	}
}
