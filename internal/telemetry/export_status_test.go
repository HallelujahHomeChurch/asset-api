package telemetry

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestExportProgressRetainedBoundedAndCumulative(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	oldWriter := log.Writer()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError + 100})))
	defer func() { slog.SetDefault(old); log.SetOutput(oldWriter) }()
	exporterState("disabled_missing_endpoint")
	p := new(exportProgress)
	now := time.Now()
	p.record(now, 2, false)
	p.record(now.Add(time.Second), 3, true)
	p.record(now.Add(time.Minute), 4, false)
	p.summary(context.Background(), true)
	p.summary(context.Background(), true)
	got := logs.String()
	for _, want := range []string{`"state":"disabled_missing_endpoint"`, `"accepted_batches_total":2`, `"accepted_spans_total":6`, `"failed_batches_total":1`, `"failed_spans_total":3`, `"shutdown_complete":true`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s: %s", want, got)
		}
	}
	if strings.Count(got, `"trigger":"batch"`) != 2 || strings.Count(got, `"trigger":"shutdown"`) != 1 {
		t.Fatal(got)
	}
}

type blockedExportHandler struct{ entered, release chan struct{} }

func (h blockedExportHandler) Enabled(context.Context, slog.Level) bool { return false }
func (h blockedExportHandler) Handle(context.Context, slog.Record) error {
	close(h.entered)
	<-h.release
	return nil
}
func (h blockedExportHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h blockedExportHandler) WithGroup(string) slog.Handler      { return h }
func TestExportSummaryCannotBlockShutdown(t *testing.T) {
	h := blockedExportHandler{make(chan struct{}), make(chan struct{})}
	old := slog.Default()
	oldWriter := log.Writer()
	slog.SetDefault(slog.New(h))
	defer func() { slog.SetDefault(old); log.SetOutput(oldWriter) }()
	p := new(exportProgress)
	start := time.Now()
	p.summary(context.Background(), false)
	if time.Since(start) > time.Second {
		t.Fatal("shutdown unbounded")
	}
	<-h.entered
	close(h.release)
	<-p.summaryDone
}
