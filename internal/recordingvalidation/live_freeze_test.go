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
