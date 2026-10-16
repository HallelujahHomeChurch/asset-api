package recordingvalidation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type diagnosticFailingObjects struct {
	PackageObjects
	stage string
	err   error
	heads int
}

func (o *diagnosticFailingObjects) Head(ctx context.Context, key string) (int64, string, error) {
	o.heads++
	if o.stage == "object_head_staging" && o.heads == 1 || o.stage == "object_head_final" && o.heads == 2 {
		return 0, "", o.err
	}
	return o.PackageObjects.Head(ctx, key)
}
func (o *diagnosticFailingObjects) CopyPackageObject(ctx context.Context, from, to, etag string) error {
	if o.stage == "object_copy" {
		return o.err
	}
	return o.PackageObjects.CopyPackageObject(ctx, from, to, etag)
}
func (o *diagnosticFailingObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if o.stage == "object_open" {
		return nil, o.err
	}
	if o.stage == "object_read" {
		return &packageErrorReader{err: o.err}, nil
	}
	return o.PackageObjects.Open(ctx, key)
}

type packageErrorReader struct{ err error }

func (r *packageErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (r *packageErrorReader) Close() error             { return nil }

func TestPackageObjectFailureStageAndRedaction(t *testing.T) {
	for _, stage := range []string{"object_head_staging", "object_copy", "object_head_final", "object_open", "object_read", "object_hash"} {
		t.Run(stage, func(t *testing.T) {
			p, store := packageTransferFixture()
			object := p.Inventory.Objects[0]
			secret := "https://provider.invalid/private/path?sig=SECRET-TOKEN"
			cause := errors.New(secret)
			if stage == "object_hash" {
				object.SHA256 = strings.Repeat("0", 64)
				cause = assets.ErrInvalidUpload
			}
			objects := &diagnosticFailingObjects{PackageObjects: store, stage: stage, err: cause}
			timing := NewAttemptTiming("package_validation")
			err := freezePackageObject(context.Background(), p, "private-final/", object, objects, io.Discard, timing.Stage)
			measuredStage := stage
			if stage == "object_open" || stage == "object_read" || stage == "object_hash" {
				measuredStage = "object_download_hash"
			}
			if timing.Snapshot().Stages[measuredStage].Calls != 1 {
				t.Fatal("failed stage duration missing")
			}
			var diagnostic *packageValidationError
			if !errors.As(err, &diagnostic) || diagnostic.stage != stage || !errors.Is(err, cause) {
				t.Fatalf("stage or cause lost: %v", err)
			}
			var output bytes.Buffer
			logPackageResult(slog.New(slog.NewJSONHandler(&output, nil)), "result", time.Now(), err)
			if !strings.Contains(output.String(), `"stage":"`+stage+`"`) || strings.Contains(output.String(), "SECRET") || strings.Contains(output.String(), "private") || strings.Contains(output.String(), "provider.invalid") {
				t.Fatalf("unsafe/missing diagnostic: %s", output.String())
			}
			want := "retry"
			if stage == "object_hash" {
				want = "invalid"
			}
			called := false
			result := RunProcessingClaim(context.Background(), func(context.Context) error { return nil }, func(context.Context) error { return err }, func(_ context.Context, ready bool, failure string) error {
				called = true
				if ready || failure != want {
					t.Fatalf("classification changed: %v %s", ready, failure)
				}
				return nil
			}, time.Hour)
			if result != nil || !called {
				t.Fatalf("finish failed: %v", result)
			}
		})
	}
}

func TestPackageStageErrorPreservesInnerStage(t *testing.T) {
	cause := errors.Join(packageStageError("decode", context.DeadlineExceeded), errors.New("private scratch path"))
	err := packageStageError("media", cause)
	var diagnostic *packageValidationError
	if !errors.As(err, &diagnostic) || diagnostic.stage != "decode" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lost stage/deadline")
	}
	var output bytes.Buffer
	logPackageResult(slog.New(slog.NewJSONHandler(&output, nil)), "result", time.Now(), err)
	if !strings.Contains(output.String(), `"reason":"deadline"`) || strings.Contains(output.String(), "private") {
		t.Fatal(output.String())
	}
}

func TestPackageProgressTextLogsNumericCounters(t *testing.T) {
	var output bytes.Buffer
	objects, segments, total, size := int64(7), int64(5), int64(12), int64(123456)
	logPackageProgress(slog.New(slog.NewTextHandler(&output, nil)), time.Now(), assets.RecordingProcessingProgress{
		Phase: "package_validation", Rendition: "720p", ObjectsVerified: &objects, SegmentsVerified: &segments, SegmentsTotal: &total, BytesVerified: &size,
	})
	for _, field := range []string{"objects_verified=7", "segments_verified=5", "segments_total=12", "bytes_verified=123456"} {
		if !strings.Contains(output.String(), field) {
			t.Fatalf("missing numeric %s: %s", field, output.String())
		}
	}
}
