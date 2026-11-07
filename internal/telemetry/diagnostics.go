package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const slowOperation = 2 * time.Second

// RecordOutcome keeps fixed-size failure evidence even when a span is not sampled.
// It never accepts error text, URLs, headers, payloads, or business identifiers.
func RecordOutcome(ctx context.Context, phase string, code int, elapsed time.Duration, failed bool) {
	recordOutcome(ctx, phase, code, elapsed, failed, slowOperation)
}

// RecordDatabaseOutcome uses the tighter database latency budget; query lifecycle includes rows consumption.
func RecordDatabaseOutcome(ctx context.Context, phase string, elapsed time.Duration, failed bool) {
	recordOutcome(ctx, phase, 0, elapsed, failed, 250*time.Millisecond)
}

func recordOutcome(ctx context.Context, phase string, code int, elapsed time.Duration, failed bool, threshold time.Duration) {
	if !failed && code < 400 && elapsed < threshold {
		return
	}
	switch phase {
	case "server", "client_headers", "client_body", "worker", "queue", "db_connect", "db_reset", "db_query_lifecycle":
	default:
		phase = "operation"
	}
	if code < 100 || code > 599 {
		code = 0
	}
	outcome := "slow"
	if failed || code >= 400 {
		outcome = "failed"
	}
	sc := trace.SpanContextFromContext(ctx)
	attrs := []any{"phase", phase, "outcome", outcome, "status", code, "duration_ms", elapsed.Milliseconds()}
	if sc.IsValid() {
		attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String(), "sampled", sc.IsSampled())
	}
	slog.ErrorContext(ctx, "operation diagnostic", attrs...)
}

type diagnosticBody struct {
	io.ReadCloser
	ctx       context.Context
	started   time.Time
	completed sync.Once
	failed    sync.Once
}

func (b *diagnosticBody) finish(err error) {
	if err != nil && !errors.Is(err, io.EOF) {
		b.failed.Do(func() {
			b.completed.Do(func() {})
			RecordOutcome(b.ctx, "client_body", 0, time.Since(b.started), true)
		})
		return
	}
	b.completed.Do(func() {
		RecordOutcome(b.ctx, "client_body", 0, time.Since(b.started), false)
	})
}
func (b *diagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish(err)
	}
	return n, err
}
func (b *diagnosticBody) Close() error { err := b.ReadCloser.Close(); b.finish(err); return err }

// Count every SDK error but emit at most one credential-free warning per minute.
// The count describes SDK errors, not spans lost or collector durability.
type exportErrors struct {
	mu    sync.Mutex
	total uint64
	last  time.Time
}

func (e *exportErrors) record(now time.Time) {
	e.mu.Lock()
	e.total++
	total := e.total
	emit := e.last.IsZero() || now.Sub(e.last) >= time.Minute
	if emit {
		e.last = now
	}
	e.mu.Unlock()
	if emit {
		slog.Error("trace export failed", "sdk_errors_total", total)
	}
}
