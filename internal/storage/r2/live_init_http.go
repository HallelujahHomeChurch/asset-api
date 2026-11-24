package r2

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// One instance per GetObject. The SDK executes attempts serially and owns retries.
type liveInitHTTPClient struct {
	client            s3.HTTPClient
	firstHeaderBudget time.Duration
}

func (c *liveInitHTTPClient) Do(r *http.Request) (*http.Response, error) {
	budget := c.firstHeaderBudget
	c.firstHeaderBudget = 0
	if budget == 0 {
		return c.client.Do(r)
	}

	ctx, cancel := context.WithCancel(r.Context())
	fired := make(chan struct{})
	timer := time.AfterFunc(budget, func() { cancel(); close(fired) })
	resp, err := c.client.Do(r.WithContext(ctx))
	if !timer.Stop() {
		<-fired
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if parentErr := r.Context().Err(); parentErr != nil {
			return nil, parentErr
		}
		// A local header timeout is retryable by the SDK. The parent is still live.
		slog.WarnContext(ctx, "recording_live_init_headers_timeout", "budget_ms", budget.Milliseconds())
		return nil, os.ErrDeadlineExceeded
	}
	if err != nil || resp == nil || resp.Body == nil {
		cancel()
		return resp, err
	}
	// After headers arrive the original parent deadline governs the entire body.
	resp.Body = &liveInitBody{resp.Body, cancel}
	return resp, nil
}

type liveInitBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *liveInitBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}
