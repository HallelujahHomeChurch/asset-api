package r2

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"hhc/asset-api/internal/telemetry"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"sync/atomic"
	"time"
)

type attemptCounterKey struct{}

// WithAttemptCounter counts actual HTTP sends, including SDK retries, for the observed operation.
func WithAttemptCounter(ctx context.Context, counter *atomic.Int64) context.Context {
	return context.WithValue(ctx, attemptCounterKey{}, counter)
}

type liveAttemptKey struct{}
type liveAttemptScope struct {
	capture, claim, rendition, stage string
	sequence                         int
}

// Bind slow transfers to a single validation attempt without logging object URLs or headers.
func WithLiveAttempt(ctx context.Context, capture, claim, rendition, stage string, seq int) context.Context {
	return context.WithValue(ctx, liveAttemptKey{}, liveAttemptScope{capture, claim, rendition, stage, seq})
}

var providerTraceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

type storageHTTPObserver struct{ client s3.HTTPClient }

func (o storageHTTPObserver) Do(r *http.Request) (*http.Response, error) {
	if counter, ok := r.Context().Value(attemptCounterKey{}).(*atomic.Int64); ok {
		counter.Add(1)
	}
	started := time.Now()
	scope, live := r.Context().Value(liveAttemptKey{}).(liveAttemptScope)
	var connected, written, firstByte atomic.Bool
	if live {
		trace := &httptrace.ClientTrace{
			GotConn:              func(httptrace.GotConnInfo) { connected.Store(true) },
			WroteRequest:         func(info httptrace.WroteRequestInfo) { written.Store(info.Err == nil) },
			GotFirstResponseByte: func() { firstByte.Store(true) },
		}
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	}
	response, err := o.client.Do(r)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if live && (err != nil || status >= 400 || time.Since(started) >= time.Second) {
		var ray, request string
		if response != nil {
			if id := response.Header.Get("Cf-Ray"); providerTraceID.MatchString(id) {
				ray = id
			}
			if id := response.Header.Get("X-Amz-Request-Id"); providerTraceID.MatchString(id) {
				request = id
			}
		}
		slog.WarnContext(r.Context(), "recording_live_storage_attempt", "capture_id", scope.capture, "claim_id", scope.claim, "sequence", scope.sequence, "rendition", scope.rendition, "stage", scope.stage, "method", r.Method, "connection_acquired", connected.Load(), "request_written", written.Load(), "first_byte", firstByte.Load(), "status", status, "cf_ray", ray, "provider_request_id", request, "elapsed_ms", time.Since(started).Milliseconds())
	}
	telemetry.RecordStorageAttempt(r.Context(), time.Since(started), status, err != nil)
	return response, err
}
