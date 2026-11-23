package r2

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync/atomic"
	"time"

	"hhc/asset-api/internal/telemetry"
)

// A reader is scoped to one GetObject. The SDK calls Do once per HTTP attempt.
type authorityHTTPReader struct {
	observer storageHTTPObserver
	start    time.Time
	attempts []authorityAttempt
}

type authorityAttempt struct {
	StartMS      int64
	ConnectionMS int64
	FirstByteMS  int64
	HeadersMS    int64
	Connection   string
	Status       int
}

func (r *authorityHTTPReader) Do(req *http.Request) (*http.Response, error) {
	start := time.Now()
	var connection atomic.Int32
	var acquired, firstByte atomic.Int64
	acquired.Store(-1)
	firstByte.Store(-1)
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			acquired.Store(time.Since(start).Milliseconds())
			if info.Reused {
				connection.Store(2)
			} else {
				connection.Store(1)
			}
		},
		GotFirstResponseByte: func() { firstByte.Store(time.Since(start).Milliseconds()) },
	}
	resp, err := r.observer.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	attempt := authorityAttempt{
		StartMS:      start.Sub(r.start).Milliseconds(),
		ConnectionMS: acquired.Load(),
		FirstByteMS:  firstByte.Load(),
		HeadersMS:    time.Since(start).Milliseconds(),
		Connection:   [...]string{"unknown", "new", "reused"}[connection.Load()],
	}
	if resp != nil {
		attempt.Status = resp.StatusCode
	}
	r.attempts = append(r.attempts, attempt)
	return resp, err
}

func (r *authorityHTTPReader) finish(ctx context.Context, stage string, body time.Duration, err error) {
	elapsed := time.Since(r.start)
	if err == nil && elapsed < time.Second {
		return
	}
	id, route := telemetry.RequestIdentity(ctx)
	state := "active"
	switch ctx.Err() {
	case context.Canceled:
		state = "canceled"
	case context.DeadlineExceeded:
		state = "deadline"
	}
	attempts := make([]slog.Attr, 0, len(r.attempts))
	for i, a := range r.attempts {
		attempts = append(attempts, slog.Group(strconv.Itoa(i+1),
			"start_ms", a.StartMS, "got_connection_ms", a.ConnectionMS,
			"first_byte_ms", a.FirstByteMS, "headers_or_error_ms", a.HeadersMS,
			"connection", a.Connection, "status", a.Status))
	}
	slog.WarnContext(ctx, "broadcast_authority_read", "request_id", id, "route_id", route, "stage", stage, "failed", err != nil, "context_state", state, "elapsed_ms", elapsed.Milliseconds(), "body_ms", body.Milliseconds(), "sends", len(r.attempts), "attempts", slog.GroupValue(attempts...))
}
