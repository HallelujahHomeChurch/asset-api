package r2

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"hhc/asset-api/internal/telemetry"
	"net/http"
	"sync/atomic"
	"time"
)

type attemptCounterKey struct{}

// WithAttemptCounter counts actual HTTP sends, including SDK retries, for a single freeze.
func WithAttemptCounter(ctx context.Context, counter *atomic.Int64) context.Context {
	return context.WithValue(ctx, attemptCounterKey{}, counter)
}

type storageHTTPObserver struct{ client s3.HTTPClient }

func (o storageHTTPObserver) Do(r *http.Request) (*http.Response, error) {
	if counter, ok := r.Context().Value(attemptCounterKey{}).(*atomic.Int64); ok {
		counter.Add(1)
	}
	started := time.Now()
	response, err := o.client.Do(r)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	telemetry.RecordStorageAttempt(r.Context(), time.Since(started), status, err != nil)
	return response, err
}
