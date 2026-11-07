package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Retain fixed operational evidence even when application log filtering is stricter.
// No endpoint, error text, attributes or other caller data may enter these records.
func exportDiagnostic(message string, attrs ...slog.Attr) {
	record := slog.NewRecord(time.Now(), slog.LevelInfo, message, 0)
	record.AddAttrs(attrs...)
	_ = slog.Default().Handler().Handle(context.Background(), record)
}

// Initialized means the SDK constructor succeeded, not collector reachability.
func exporterState(state string) {
	exportDiagnostic("trace_exporter_state", slog.String("state", state))
}

type exportProgress struct {
	mu                                                         sync.Mutex
	acceptedBatches, acceptedSpans, failedBatches, failedSpans uint64
	last                                                       time.Time
	summaryOnce                                                sync.Once
	summaryDone                                                chan struct{}
}

func (p *exportProgress) attrsLocked(trigger string) []slog.Attr {
	return []slog.Attr{slog.String("boundary", "otlp_export_call"), slog.String("trigger", trigger), slog.Uint64("accepted_batches_total", p.acceptedBatches), slog.Uint64("accepted_spans_total", p.acceptedSpans), slog.Uint64("failed_batches_total", p.failedBatches), slog.Uint64("failed_spans_total", p.failedSpans)}
}

// Accepted means a successful OTLP export call, not storage durability. Failed
// calls may have reached the collector; these counts are not dropped-span counts.
func (p *exportProgress) record(now time.Time, spans int, failed bool) {
	if p == nil || spans == 0 {
		return
	}
	p.mu.Lock()
	if failed {
		p.failedBatches++
		p.failedSpans += uint64(spans)
	} else {
		p.acceptedBatches++
		p.acceptedSpans += uint64(spans)
	}
	emit := p.last.IsZero() || now.Sub(p.last) >= time.Minute
	var attrs []slog.Attr
	if emit {
		p.last = now
		attrs = p.attrsLocked("batch")
	}
	p.mu.Unlock()
	if emit {
		exportDiagnostic("trace_export_progress", attrs...)
	}
}

// Startup and periodic evidence use the existing synchronous sink, best effort.
// Only shutdown owns a one-shot goroutine so a stuck sink cannot delay shutdown.
func (p *exportProgress) summary(ctx context.Context, complete bool) {
	p.summaryOnce.Do(func() {
		p.summaryDone = make(chan struct{})
		p.mu.Lock()
		attrs := p.attrsLocked("shutdown")
		p.mu.Unlock()
		attrs = append(attrs, slog.Bool("shutdown_complete", complete))
		go func() {
			defer close(p.summaryDone)
			exportDiagnostic("trace_export_progress", attrs...)
		}()
	})
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.summaryDone:
	case <-ctx.Done():
	case <-timer.C:
	}
}
