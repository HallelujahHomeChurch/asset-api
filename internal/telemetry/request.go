package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var phaseNames = [...]string{"db_connect", "db_reset", "db_query_lifecycle", "metadata", "storage_response", "storage_attempt", "storage_retry", "storage_body", "stream_copy", "client_headers", "client_body"}

type phaseValue struct {
	count      uint64
	total, max time.Duration
	failures   uint64
	status     int
}
type phaseSummary struct {
	Phase    string  `json:"phase"`
	Count    uint64  `json:"count"`
	TotalMS  float64 `json:"total_ms"`
	MaxMS    float64 `json:"max_ms"`
	Failures uint64  `json:"failures"`
	Status   int     `json:"status,omitempty"`
}
type requestObservation struct {
	mu                sync.Mutex
	values            [len(phaseNames)]phaseValue
	closed, failed    bool
	id, route, reason string
	parentSpanID      string
}
type requestKey struct{}

var diagnosticID = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[47][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$`)

func observeRequest(ctx context.Context, r *http.Request) context.Context {
	id := r.Header.Get("X-HHC-Request-ID")
	if len(r.Header.Values("X-HHC-Request-ID")) != 1 || !diagnosticID.MatchString(id) {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("request ID entropy unavailable")
		}
		id = hex.EncodeToString(b[:])
	}
	route := "other"
	parts := strings.Split(r.URL.Path, "/")
	if (r.Method == "GET" || r.Method == "HEAD") && len(parts) >= 4 && parts[1] == "priv" && parts[2] == "recording-packages" && parts[3] != "" {
		switch {
		case len(parts) == 4:
			route = "recording_package_read"
		case len(parts) == 5 && parts[4] == "ready":
			route = "recording_package_ready"
		}
	}
	if (r.Method == "GET" || r.Method == "HEAD") && len(parts) >= 5 && len(parts) <= 6 && parts[1] == "api" && parts[2] == "assets" && parts[3] == "public" {
		route = "asset_public_download"
	}
	if r.Method == "DELETE" && len(parts) == 6 && parts[1] == "priv" && parts[2] == "assets" && parts[4] == "grants" {
		route = "asset_grant_revoke"
	}
	return context.WithValue(ctx, requestKey{}, &requestObservation{id: id, route: route, parentSpanID: trace.SpanContextFromContext(ctx).SpanID().String()})
}

// These are overlapping work durations, never an additive request latency.
func RecordPhase(ctx context.Context, phase string, elapsed time.Duration, failed bool) {
	recordPhase(ctx, phase, elapsed, failed, 0)
}
func recordPhase(ctx context.Context, phase string, elapsed time.Duration, failed bool, status int) {
	o, _ := ctx.Value(requestKey{}).(*requestObservation)
	if o == nil {
		return
	}
	for i, name := range phaseNames {
		if phase != name {
			continue
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.closed {
			return
		}
		v := &o.values[i]
		v.count++
		v.total += max(0, elapsed)
		v.max = max(v.max, elapsed)
		if failed {
			v.failures++
			o.failed = true
			if o.reason == "" {
				o.reason = name + "_failed"
			}
		}
		if status >= 100 && status <= 599 {
			v.status = status
		}
		return
	}
}
func (o *requestObservation) finish() ([]phaseSummary, bool, bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, false, false, ""
	}
	o.closed = true
	out := make([]phaseSummary, 0, len(phaseNames))
	for i, v := range o.values {
		if v.count > 0 {
			out = append(out, phaseSummary{phaseNames[i], v.count, float64(v.total) / float64(time.Millisecond), float64(v.max) / float64(time.Millisecond), v.failures, v.status})
		}
	}
	return out, o.failed, true, o.reason
}
func setFailureReason(ctx context.Context, reason string) bool {
	o, _ := ctx.Value(requestKey{}).(*requestObservation)
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.reason = reason
		o.failed = true
	}
	return true
}

type storageKey struct{}
type storageCall struct{ attempts atomic.Int32 }

// StorageCall scopes SDK attempts/retries to one operation without changing its context deadline.
func StorageCall(ctx context.Context, phase string) (context.Context, func(error)) {
	ctx = context.WithValue(ctx, storageKey{}, new(storageCall))
	started := time.Now()
	var once sync.Once
	return ctx, func(err error) { once.Do(func() { RecordPhase(ctx, phase, time.Since(started), err != nil) }) }
}
func RecordStorageAttempt(ctx context.Context, elapsed time.Duration, status int, failed bool) {
	call, _ := ctx.Value(storageKey{}).(*storageCall)
	if call == nil {
		return
	}
	if call.attempts.Add(1) > 1 {
		RecordPhase(ctx, "storage_retry", 0, false)
	}
	recordPhase(ctx, "storage_attempt", elapsed, failed || status >= 400, status)
}
func StorageBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	return &storageBody{ReadCloser: body, ctx: ctx, started: time.Now()}
}

type storageBody struct {
	io.ReadCloser
	ctx     context.Context
	started time.Time
	once    sync.Once
	eof     atomic.Bool
}

func (b *storageBody) finish(err error, early bool) {
	if err != nil && err != io.EOF {
		setFailureReason(b.ctx, "storage_body_error")
	}
	b.once.Do(func() {
		failed := err != nil && err != io.EOF || early
		RecordPhase(b.ctx, "storage_body", time.Since(b.started), failed)
		if failed {
			reason := "storage_body_error"
			if early {
				reason = "storage_body_closed_early"
			}
			setFailureReason(b.ctx, reason)
		}
	})
}
func (b *storageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.eof.Store(true)
	}
	if err != nil {
		b.finish(err, false)
	}
	return n, err
}
func (b *storageBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish(err, !b.eof.Load())
	return err
}

// Groups keep the same named numeric fields under JSON and production text handlers.
func phaseLogValue(values []phaseSummary) slog.Value {
	attrs := make([]slog.Attr, 0, len(values))
	for _, v := range values {
		attrs = append(attrs, slog.Group(v.Phase, "count", v.Count, "total_ms", v.TotalMS, "max_ms", v.MaxMS, "failures", v.Failures, "status", v.Status))
	}
	return slog.GroupValue(attrs...)
}
