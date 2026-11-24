package recordingvalidation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// One observer per rendition. Object names, URLs and provider errors stay private.
type liveFreeze struct {
	PackageObjects
	part                    string
	lastOK, failed, slowest string
	longest                 time.Duration
	readTime, writeTime     time.Duration
	received                int64
	lastRead                time.Time
	attempts                atomic.Int64
}

func (f *liveFreeze) record(stage string, started time.Time, err error) {
	stage = f.part + "." + stage
	if elapsed := time.Since(started); elapsed > f.longest {
		f.slowest, f.longest = stage, elapsed
	}
	if f.failed != "" {
		return
	}
	if err == nil || errors.Is(err, io.EOF) {
		f.lastOK = stage
	} else {
		f.failed = stage
	}
}

func (f *liveFreeze) Head(ctx context.Context, key string) (int64, string, error) {
	stage := "head_final"
	if strings.Contains(key, "/staging/") {
		stage = "head_staging"
	}
	start := time.Now()
	size, etag, err := f.PackageObjects.Head(ctx, key)
	f.record(stage, start, err)
	return size, etag, err
}

func (f *liveFreeze) CopyPackageObject(ctx context.Context, from, to, etag string) error {
	start := time.Now()
	err := f.PackageObjects.CopyPackageObject(ctx, from, to, etag)
	f.record("copy", start, err)
	return err
}

func (f *liveFreeze) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	start := time.Now()
	open := f.PackageObjects.Open
	if initReader, ok := f.PackageObjects.(interface {
		OpenLiveInit(context.Context, string) (io.ReadCloser, error)
	}); ok && f.part == "init" {
		open = initReader.OpenLiveInit
	}
	body, err := open(ctx, key)
	f.record("get_headers", start, err)
	if err != nil {
		return nil, err
	}
	return &freezeBody{body, f}, nil
}

type freezeBody struct {
	io.ReadCloser
	f *liveFreeze
}

func (b *freezeBody) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.f.received += int64(n)
		b.f.lastRead = time.Now()
	}
	b.f.readTime += time.Since(start)
	b.f.record("read_body", start, err)
	return n, err
}

func (b *freezeBody) Close() error {
	start := time.Now()
	err := b.ReadCloser.Close()
	b.f.record("close_body", start, err)
	return err
}

type freezeWriter struct {
	io.Writer
	f *liveFreeze
}

func (w freezeWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := w.Writer.Write(p)
	w.f.writeTime += time.Since(start)
	w.f.record("write_scratch", start, err)
	return n, err
}

func (f *liveFreeze) report(pack, claim, rendition string, seq int, elapsed time.Duration, err, parentErr error) {
	if elapsed < 5*time.Second && err == nil {
		return
	}
	stage, reason := packageFailureDiagnostic(err)
	_, parentReason := packageFailureDiagnostic(parentErr)
	idle := int64(-1)
	if !f.lastRead.IsZero() {
		idle = time.Since(f.lastRead).Milliseconds()
	}
	slog.Warn("recording_live_freeze", "capture_id", pack, "claim_id", claim, "sequence", seq, "rendition", rendition,
		"elapsed_ms", elapsed.Milliseconds(), "last_success", f.lastOK, "failed_operation", f.failed, "validation_stage", stage, "reason", reason, "parent_reason", parentReason,
		"slowest_operation", f.slowest, "slowest_ms", f.longest.Milliseconds(), "read_ms", f.readTime.Milliseconds(), "scratch_write_ms", f.writeTime.Milliseconds(), "bytes_read", f.received, "last_read_ago_ms", idle, "http_attempts", f.attempts.Load())
}
