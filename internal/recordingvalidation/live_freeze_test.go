package recordingvalidation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type stalledSegment struct{ PackageObjects }

func (s stalledSegment) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasSuffix(key, ".m4s") {
		return io.NopCloser(io.MultiReader(strings.NewReader("part"), &packageErrorReader{err: errors.New("private-provider-path")})), nil
	}
	return s.PackageObjects.Open(ctx, key)
}
func TestLiveFreezeReportsPartialReadBeforeDecode(t *testing.T) {
	p, objects, decl := liveIntakeFixture()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	_, err := (PackageMediaProbe{Objects: stalledSegment{objects}, FFmpeg: "/unused", FFprobe: "/unused", ScratchRoot: t.TempDir()}).ValidateLiveSegment(t.Context(), p.ID, "incident", 0, decl)
	if err == nil {
		t.Fatal("partial read accepted")
	}
	logs := buf.String()
	for _, want := range []string{"msg=recording_live_freeze", "claim_id=incident", "failed_operation=segment.read_body", "bytes_read=19", "probe_decode_ms=0"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q: %s", want, logs)
		}
	}
	if strings.Contains(logs, "private-provider-path") {
		t.Fatal("provider detail logged")
	}
}

type routedFreezeReads struct {
	PackageObjects
	keys []string
}

func (r *routedFreezeReads) OpenLiveInit(ctx context.Context, key string) (io.ReadCloser, error) {
	r.keys = append(r.keys, "init:"+key)
	return io.NopCloser(strings.NewReader("init")), nil
}

func (r *routedFreezeReads) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	r.keys = append(r.keys, "ordinary:"+key)
	return io.NopCloser(strings.NewReader("segment")), nil
}

func TestFreezeOnlyInitializationUsesHeaderRecovery(t *testing.T) {
	store := new(routedFreezeReads)
	f := liveFreeze{PackageObjects: store}
	for _, part := range []string{"init", "segment"} {
		f.part = part
		body, err := f.Open(t.Context(), part)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(body)
		if closeErr := body.Close(); err != nil || closeErr != nil || string(got) != part {
			t.Fatalf("part=%s got=%q read=%v close=%v", part, got, err, closeErr)
		}
	}
	if strings.Join(store.keys, ",") != "init:init,ordinary:segment" {
		t.Fatalf("read routes: %v", store.keys)
	}
}
