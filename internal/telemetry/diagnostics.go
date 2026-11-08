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

type auditOperationKey struct{}

// WithAuditVerification labels the existing empty-body authentication probe.
func WithAuditVerification(ctx context.Context) context.Context {
	return context.WithValue(ctx, auditOperationKey{}, "audit_auth_probe")
}

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
	var summaries []phaseSummary
	observation, _ := ctx.Value(requestKey{}).(*requestObservation)
	reason := ""
	if phase == "server" {
		if observation != nil {
			var emit, childFailed bool
			summaries, childFailed, emit, reason = observation.finish()
			if !emit {
				return
			}
			failed = failed || childFailed
		}
	} else {
		recordPhase(ctx, phase, elapsed, failed || code >= 400, code)
	}
	if ctx.Err() != nil {
		failed = true
		if reason == "" {
			reason = "caller_canceled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "local_deadline"
			}
		}
	}
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
	if outcome == "failed" {
		state := "active"
		if errors.Is(ctx.Err(), context.Canceled) {
			state = "canceled"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			state = "deadline"
		}
		// Context state is evidence at logging time, not the database error's cause.
		attrs = append(attrs, "context_state", state)
	}
	if operation, _ := ctx.Value(auditOperationKey{}).(string); operation == "audit_auth_probe" || operation == "audit_append" {
		attrs = append(attrs, "operation", operation)
	}
	if observation != nil {
		attrs = append(attrs, "request_id", observation.id, "route_id", observation.route)
		if phase == "server" {
			attrs = append(attrs, "parent_span_id", observation.parentSpanID, "operation", "http_request", "phase_totals", phaseLogValue(summaries), "failure_reason", reason, "timing_semantics", "overlapping_work_not_wall_time", "db_checkout", "unmeasured_implicit_checkout")
		}
	}
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

// RecordFailureReason accepts only closed audit categories, never error text.
func RecordFailureReason(ctx context.Context, reason string) {
	switch reason {
	case "audit_provenance_missing", "audit_event_invalid", "audit_enqueue_failed":
	default:
		return
	}
	if setFailureReason(ctx, reason) {
		return
	}
	sc := trace.SpanContextFromContext(ctx)
	slog.ErrorContext(ctx, "audit mutation unavailable", "failure_reason", reason, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}
