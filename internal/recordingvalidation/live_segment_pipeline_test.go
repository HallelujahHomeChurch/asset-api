package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

// Serial intake cannot start the sibling; unbounded intake opens a third file.
// Either failure must join cancelled siblings before removing their scratch.
func TestLiveSegmentBoundedIntakeJoinsCancelledSibling(t *testing.T) {
	for _, externalCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider failure", true: "caller cancelled"}[externalCancel], func(t *testing.T) {
			p, objects, declarations := liveIntakeFixture()
			failure := errors.New("provider interrupted")
			blocked := &blockedLiveObjects{packageMemoryObjects: objects, opened: make(chan string, 3), release: make(chan struct{}), failure: failure}
			probe := PackageMediaProbe{Objects: blocked, FFmpeg: "/usr/bin/false", FFprobe: "/usr/bin/false", ScratchRoot: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			result := make(chan error, 1)
			go func() {
				batch, err := probe.ValidateLiveSegment(ctx, p.ID, "bounded", 0, declarations)
				if batch != nil {
					err = errors.New("failed batch exposed partial results")
				}
				result <- err
				close(result)
			}()
			defer func() {
				cancel()
				for range result {
				}
			}()
			for i := 0; i < 2; i++ {
				select {
				case <-blocked.opened:
				case <-ctx.Done():
					t.Fatal("second rendition did not start concurrently")
				}
			}
			select {
			case name := <-blocked.opened:
				t.Fatalf("third rendition exceeded two-worker budget: %s", name)
			case <-time.After(50 * time.Millisecond):
			}
			if externalCancel {
				cancel()
				failure = context.Canceled
			} else {
				close(blocked.release)
			}
			err := <-result
			if !errors.Is(err, failure) {
				t.Fatalf("root error lost: got %v want %v", err, failure)
			}
			if blocked.active.Load() != 0 {
				t.Fatal("returned before provider sibling stopped")
			}
			entries, err := os.ReadDir(probe.ScratchRoot)
			if err != nil || len(entries) != 0 || objects.puts != 0 {
				t.Fatalf("failed intake left scratch or published inventory: %v puts=%d err=%v", entries, objects.puts, err)
			}
		})
	}
}

func liveIntakeFixture() (assets.RecordingPackage, *packageMemoryObjects, []assets.RecordingPackageObject) {
	p, objects := packageTransferFixture()
	var declarations []assets.RecordingPackageObject
	for _, r := range assets.LiveRenditions() {
		for _, name := range []string{"init.mp4", "seg-000000.m4s"} {
			data := []byte("immutable bytes")
			hash := sha256.Sum256(data)
			path := r.Name + "/" + name
			objects.bytes[p.StagingKey(path)] = data
			declarations = append(declarations, assets.RecordingPackageObject{Path: path, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
		}
	}
	return p, objects, declarations
}

func TestLiveSegmentCancellationReapsDecodersBeforeScratchRemoval(t *testing.T) {
	p, objects, declarations := liveIntakeFixture()
	tools := t.TempDir()
	pids := filepath.Join(tools, "children")
	probe := PackageMediaProbe{Objects: objects, FFmpeg: filepath.Join(tools, "decoder"), FFprobe: filepath.Join(tools, "probe"), ScratchRoot: t.TempDir()}
	if err := os.WriteFile(probe.FFmpeg, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$$\" >> %q\nexec sleep 30\n", pids)), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \" $* \" in *-read_intervals*) cat <<'JSON'\n" + `{"packets":[{"flags":"K_","data":"\n00000000: 0000 0002 6500                         ......\n"}]}` + "\nJSON\n;; *) case \"$*\" in\n"
	for _, r := range assets.LiveRenditions() {
		data := strings.ReplaceAll(segmentProbeFixture, `"width":1280,"height":720`, fmt.Sprintf(`"width":%d,"height":%d`, r.Width, r.Height))
		script += "*" + r.Name + ".mp4*) cat <<'JSON'\n" + data + "\nJSON\n;;\n"
	}
	script += "esac\n;; esac\n"
	if err := os.WriteFile(probe.FFprobe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result := make(chan error, 1)
	go func() {
		_, err := probe.ValidateLiveSegment(ctx, p.ID, "decoder-cancel", 0, declarations)
		result <- err
		close(result)
	}()
	defer func() {
		cancel()
		for range result {
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var children []string
	for len(children) < 2 {
		data, err := os.ReadFile(pids)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		children = strings.Fields(string(data))
		if len(children) >= 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("both decoders did not start")
		}
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation lost: %v", err)
	}
	for _, value := range children {
		pid, err := strconv.Atoi(value)
		if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			t.Fatalf("decoder child still alive: %q", value)
		}
	}
	entries, err := os.ReadDir(probe.ScratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch retained: %v %v", entries, err)
	}
}

type blockedLiveObjects struct {
	*packageMemoryObjects
	opened  chan string
	release chan struct{}
	failure error
	active  atomic.Int64
}

func (s *blockedLiveObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	s.active.Add(1)
	defer s.active.Add(-1)
	s.opened <- key
	if strings.Contains(key, "/1080p/") {
		select {
		case <-s.release:
			return nil, s.failure
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
