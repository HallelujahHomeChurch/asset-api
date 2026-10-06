package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"hhc/asset-api/internal/assets"
)

type packageMemoryObjects struct {
	mu                                      sync.Mutex
	bytes                                   map[string][]byte
	inventory                               []byte
	heads, copies, opens, puts, openedBytes int64
}

func (s *packageMemoryObjects) Head(_ context.Context, key string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heads++
	b, ok := s.bytes[key]
	if !ok {
		return 0, "", assets.ErrNotFound
	}
	return int64(len(b)), "etag", nil
}
func (s *packageMemoryObjects) CopyPackageObject(_ context.Context, from, to, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.copies++
	s.bytes[to] = bytes.Clone(s.bytes[from])
	return nil
}
func (s *packageMemoryObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	s.openedBytes += int64(len(s.bytes[key]))
	return io.NopCloser(bytes.NewReader(bytes.Clone(s.bytes[key]))), nil
}
func (s *packageMemoryObjects) PutPackageInventory(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.inventory = bytes.Clone(data)
	s.bytes[key] = bytes.Clone(data)
	return nil
}

func packageTransferFixture() (assets.RecordingPackage, *packageMemoryObjects) {
	inv := assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1", Renditions: []assets.RecordingRendition{{Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}}}
	content := map[string][]byte{
		"master.m3u8":     []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=12,AVERAGE-BANDWIDTH=12,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\n720p/index.m3u8\n"),
		"720p/index.m3u8": []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:5.000000,\nseg-000000.m4s\n#EXT-X-ENDLIST\n"),
		"720p/init.mp4":   []byte("init"), "720p/seg-000000.m4s": []byte("segment"),
	}
	store := &packageMemoryObjects{bytes: map[string][]byte{}}
	for path, data := range content {
		hash := sha256.Sum256(data)
		inv.Objects = append(inv.Objects, assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
		store.bytes["recordings/packages/package-a/staging/"+path] = data
	}
	inv.InventoryDigest, _ = assets.RecordingInventoryDigest(inv)
	size, _ := assets.ValidateRecordingInventory(inv)
	return assets.RecordingPackage{ID: "package-a", SizeBytes: size, Inventory: inv}, store
}

func TestImmutablePackageFinalSurvivesLateStagingPut(t *testing.T) {
	p, objects := packageTransferFixture()
	prefix, err := FreezeRecordingPackage(context.Background(), p, "attempt-a", objects, func(_ context.Context, inv assets.RecordingPackageInventory, prefix string) error {
		objects.bytes[p.StagingKey("720p/seg-000000.m4s")] = []byte("changed")
		if string(objects.bytes[prefix+"720p/seg-000000.m4s"]) != "segment" {
			t.Fatal("late PUT changed the copy being probed")
		}
		return nil
	})
	if err != nil || prefix != "recordings/packages/package-a/final/attempt-a/" {
		t.Fatalf("freeze: %s %v", prefix, err)
	}
	if !bytes.Contains(objects.inventory, []byte(p.Inventory.InventoryDigest)) {
		t.Fatal("control inventory not persisted")
	}
}

func TestPackageHashFailureNeverReachesMediaProbe(t *testing.T) {
	p, objects := packageTransferFixture()
	objects.bytes[p.StagingKey("720p/seg-000000.m4s")] = []byte("changed")
	_, err := FreezeRecordingPackage(context.Background(), p, "attempt-a", objects, func(context.Context, assets.RecordingPackageInventory, string) error {
		t.Fatal("invalid bytes reached media probe")
		return nil
	})
	if !errors.Is(err, assets.ErrInvalidUpload) || objects.inventory != nil {
		t.Fatalf("hash failure accepted: %v", err)
	}
	_, err = FreezeRecordingPackage(context.Background(), p, "attempt-a", objects, nil)
	if !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("missing media probe accepted: %v", err)
	}
	_, err = FreezeRecordingPackage(context.Background(), p, "../escape", objects, func(context.Context, assets.RecordingPackageInventory, string) error { return nil })
	if !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("unsafe attempt: %v", err)
	}
}

func TestPackageUnsafePlaylistRejectedBeforeProbe(t *testing.T) {
	p, objects := packageTransferFixture()
	key := p.StagingKey("720p/index.m3u8")
	objects.bytes[key] = []byte(strings.Replace(string(objects.bytes[key]), `URI="init.mp4"`, `URI="https://evil/init.mp4"`, 1))
	for i, o := range p.Inventory.Objects {
		if o.Path == "720p/index.m3u8" {
			hash := sha256.Sum256(objects.bytes[key])
			p.Inventory.Objects[i].SHA256 = hex.EncodeToString(hash[:])
			p.Inventory.Objects[i].SizeBytes = int64(len(objects.bytes[key]))
		}
	}
	p.Inventory.InventoryDigest, _ = assets.RecordingInventoryDigest(p.Inventory)
	p.SizeBytes, _ = assets.ValidateRecordingInventory(p.Inventory)
	_, err := FreezeRecordingPackage(context.Background(), p, "attempt-a", objects, func(context.Context, assets.RecordingPackageInventory, string) error {
		t.Fatal("unsafe URI reached media probe")
		return nil
	})
	if !errors.Is(err, assets.ErrInvalidUpload) {
		t.Fatalf("unsafe playlist accepted: %v", err)
	}
}
