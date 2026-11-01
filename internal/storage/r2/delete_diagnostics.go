package r2

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Observe only counts and timing: provider errors can contain private object keys.
func (s *Store) deleteObjects(ctx context.Context, objects []types.ObjectIdentifier) (result *s3.DeleteObjectsOutput, err error) {
	started := time.Now()
	client := &deleteHTTPObserver{client: s.client.Options().HTTPClient, objects: len(objects)}
	defer func() {
		partial := 0
		if result != nil {
			partial = len(result.Errors)
		}
		slog.Info("recording_delete_batch", "objects", len(objects), "attempts", client.attempts, "elapsed_ms", time.Since(started).Milliseconds(), "partial_errors", partial, "failed", err != nil || partial > 0, "deadline_exceeded", errors.Is(err, context.DeadlineExceeded))
	}()
	return s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)}}, func(options *s3.Options) { options.HTTPClient = client })
}

type deleteHTTPObserver struct {
	client   s3.HTTPClient
	objects  int
	attempts int
}

func (o *deleteHTTPObserver) Do(request *http.Request) (*http.Response, error) {
	o.attempts++
	started := time.Now()
	response, err := o.client.Do(request)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	// Do ends at response headers; the batch event also includes body decoding and retries.
	slog.Info("recording_delete_attempt", "objects", o.objects, "attempt", o.attempts, "elapsed_ms", time.Since(started).Milliseconds(), "status_code", status, "failed", err != nil || status >= 400, "deadline_exceeded", errors.Is(err, context.DeadlineExceeded))
	return response, err
}
