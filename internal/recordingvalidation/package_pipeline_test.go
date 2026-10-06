package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestPackagePipelineActualFrameRates(t *testing.T) {
	for _, frameRate := range []int{2, 24} {
		t.Run(fmt.Sprint(frameRate), func(t *testing.T) {
			p, objects, probe := realPackageFixtureAtFrameRate(t, frameRate)
			if _, err := FreezeRecordingPackage(context.Background(), p, "frame-rate", objects, probe); err != nil {
				t.Fatal(err)
			}
			if objects.opens != int64(len(p.Inventory.Objects)) || objects.puts != 1 {
				t.Fatalf("frame rate %d: GET=%d inventory PUT=%d", frameRate, objects.opens, objects.puts)
			}
		})
	}
}

func TestPackagePipelineSingleReadAndBound(t *testing.T) {
	p, objects, probe := realPackageFixture(t)
	observed := &orderedPackageObjects{packageMemoryObjects: objects, secondClosed: make(chan struct{}), firstOpened: make(chan struct{})}
	if _, err := FreezeRecordingPackage(context.Background(), p, "single-read", observed, probe); err != nil {
		t.Fatal(err)
	}
	var objectBytes int64
	for _, object := range p.Inventory.Objects {
		objectBytes += object.SizeBytes
	}
	if objects.opens != 5 || objects.openedBytes != objectBytes {
		t.Fatalf("duplicate final reads: GET=%d bytes=%d want GET=5 bytes=%d", objects.opens, objects.openedBytes, objectBytes)
	}
	if objects.copies != 5 || objects.heads != 10 || objects.puts != 1 {
		t.Fatalf("object graph not frozen once: copy=%d head=%d inventory=%d", objects.copies, objects.heads, objects.puts)
	}
	if observed.max.Load() != 2 || observed.active.Load() != 0 {
		t.Fatalf("worker bound: max=%d active=%d", observed.max.Load(), observed.active.Load())
	}
	entries, err := os.ReadDir(probe.ScratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch retained: %v %v", entries, err)
	}
}

type orderedPackageObjects struct {
	*packageMemoryObjects
	active, max  atomic.Int64
	secondClosed chan struct{}
	firstOpened  chan struct{}
	once         sync.Once
}

type observedPackageReader struct {
	io.ReadCloser
	close      func()
	beforeRead func() error
}

func (r *observedPackageReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		if err := r.beforeRead(); err != nil {
			return 0, err
		}
		r.beforeRead = nil
	}
	return r.ReadCloser.Read(p)
}
func (r *observedPackageReader) Close() error { err := r.ReadCloser.Close(); r.close(); return err }

func (s *orderedPackageObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	body, err := s.packageMemoryObjects.Open(ctx, key)
	if err != nil || !strings.HasSuffix(key, ".m4s") {
		return body, err
	}
	n := s.active.Add(1)
	for old := s.max.Load(); n > old; old = s.max.Load() {
		if s.max.CompareAndSwap(old, n) {
			break
		}
	}
	r := &observedPackageReader{ReadCloser: body, close: func() {
		s.active.Add(-1)
		if strings.HasSuffix(key, "seg-000001.m4s") {
			s.once.Do(func() { close(s.secondClosed) })
		}
	}}
	if strings.HasSuffix(key, "seg-000000.m4s") {
		close(s.firstOpened)
		r.beforeRead = func() error {
			select {
			case <-s.secondClosed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	} else {
		r.beforeRead = func() error {
			select {
			case <-s.firstOpened:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return r, nil
}

func TestPackagePipelineCancellationAndBudget(t *testing.T) {
	p, objects, probe := realPackageFixture(t)
	providerFailure := errors.New("provider read interrupted")
	failed := &failingPackageObjects{packageMemoryObjects: objects, failure: providerFailure, secondStarted: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := FreezeRecordingPackage(ctx, p, "failed-read", failed, probe); !errors.Is(err, providerFailure) {
		t.Fatalf("root error lost: %v", err)
	}
	if failed.active.Load() != 0 || objects.puts != 0 {
		t.Fatalf("unfinished sibling or inventory published: active=%d puts=%d", failed.active.Load(), objects.puts)
	}
	entries, err := os.ReadDir(probe.ScratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch retained on failure: %v %v", entries, err)
	}
	if err := checkFragmentScratch(probe.ScratchRoot, (1<<30)+1); err == nil {
		t.Fatal("scratch cap not enforced")
	}
}

type failingPackageObjects struct {
	*packageMemoryObjects
	failure       error
	secondStarted chan struct{}
	active        atomic.Int64
}

func (s *failingPackageObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasSuffix(key, "seg-000000.m4s") {
		select {
		case <-s.secondStarted:
			return nil, s.failure
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if strings.HasSuffix(key, "seg-000001.m4s") {
		s.active.Add(1)
		defer s.active.Add(-1)
		close(s.secondStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.packageMemoryObjects.Open(ctx, key)
}

func TestPackagePipelineRejectsInvalidMedia(t *testing.T) {
	r := assets.RecordingRendition{Name: "720p", FrameRate: 30, DurationSeconds: 35, SegmentCount: 2}
	valid := []segmentProbe{{Start: 0, End: 30, Codecs: "avc1.42c01f,mp4a.40.2"}, {Start: 30, End: 35, Codecs: "avc1.42c01f,mp4a.40.2"}}
	for name, mutate := range map[string]func([]segmentProbe){
		"gap":         func(s []segmentProbe) { s[1].Start += 0.1 },
		"codec":       func(s []segmentProbe) { s[1].Codecs = "avc1.64001f,mp4a.40.2" },
		"tail":        func(s []segmentProbe) { s[1].End += 0.1 },
		"wrong start": func(s []segmentProbe) { s[0].Start = 0.5 },
	} {
		t.Run(name, func(t *testing.T) {
			s := append([]segmentProbe(nil), valid...)
			mutate(s)
			if !errors.Is(validateRenditionTimeline(r, s, nil), assets.ErrInvalidUpload) {
				t.Fatal("invalid timeline accepted")
			}
		})
	}
	other := append([]segmentProbe(nil), valid...)
	other[1].End += 0.1
	if !errors.Is(validateRenditionTimeline(r, valid, other), assets.ErrInvalidUpload) {
		t.Fatal("cross rendition offset accepted")
	}
}
