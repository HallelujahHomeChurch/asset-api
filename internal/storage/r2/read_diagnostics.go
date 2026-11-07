package r2

import (
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"hhc/asset-api/internal/telemetry"
	"net/http"
	"time"
)

type storageHTTPObserver struct{ client s3.HTTPClient }

func (o storageHTTPObserver) Do(r *http.Request) (*http.Response, error) {
	started := time.Now()
	response, err := o.client.Do(r)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	telemetry.RecordStorageAttempt(r.Context(), time.Since(started), status, err != nil)
	return response, err
}
