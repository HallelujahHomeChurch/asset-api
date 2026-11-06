package recordingvalidation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type archiveCopyFailure struct {
	PackageObjects
	failure error
}

func (s archiveCopyFailure) CopyPackageObject(ctx context.Context, from, to, etag string) error {
	if strings.HasPrefix(from, "recordings/captures/") {
		return s.failure
	}
	return s.PackageObjects.CopyPackageObject(ctx, from, to, etag)
}

type archiveCopyGate struct {
	PackageObjects
	started chan struct{}
	release chan struct{}
}

func (s archiveCopyGate) CopyPackageObject(ctx context.Context, from, to, etag string) error {
	if strings.HasPrefix(from, "recordings/captures/") {
		s.started <- struct{}{}
		<-s.release
	}
	return s.PackageObjects.CopyPackageObject(ctx, from, to, etag)
}
func assertArchiveFailureAndCancellation(t *testing.T, p assets.RecordingPackage, history []assets.RecordingLiveSegment, objects *packageMemoryObjects) {
	t.Helper()
	puts := objects.puts
	failure := errors.New("conditional copy rejected")
	if _, used, err := freezeLiveArchive(context.Background(), p, "copy-failed", history, archiveCopyFailure{objects, failure}, PackageMediaProbe{}); !used || !errors.Is(err, failure) {
		t.Fatalf("copy failure published: %v %v", used, err)
	}
	if objects.puts != puts {
		t.Fatal("failed copy wrote package inventory")
	}
	gate := archiveCopyGate{objects, make(chan struct{}, 16), make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := freezeLiveArchive(ctx, p, "cancel-copy", history, gate, PackageMediaProbe{})
		done <- err
	}()
	for i := 0; i < 6; i++ {
		select {
		case <-gate.started:
		case <-ctx.Done():
			close(gate.release)
			t.Fatal("copies did not reach bounded parallelism")
		}
	}
	select {
	case <-gate.started:
		close(gate.release)
		t.Fatal("more than six active copies")
	default:
	}
	cancel()
	select {
	case <-done:
		close(gate.release)
		t.Fatal("returned before provider operations joined")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if objects.puts != puts {
		t.Fatal("cancelled copy wrote package inventory")
	}
}
