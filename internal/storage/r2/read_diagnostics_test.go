package r2

import (
	"bytes"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"hhc/asset-api/internal/telemetry"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestR2DownloadRecordsRealSDKRetriesAndBody(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(503)
			io.WriteString(w, `<Error><Code>ServiceUnavailable</Code></Error>`)
			return
		}
		io.WriteString(w, "bytes")
	}))
	defer server.Close()
	store := &Store{bucket: "private-bucket", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), Retryer: retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2; o.MaxBackoff = time.Millisecond })})}
	telemetry.Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := store.Open(r.Context(), "recordings/packages/private/master.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		if err != nil || string(data) != "bytes" {
			t.Fatal(err)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(200)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/assets/public/synthetic", nil))
	if calls != 2 {
		t.Fatal("SDK retry changed", calls)
	}
	for _, want := range []string{`"phase":"storage_attempt","count":2`, `"phase":"storage_retry","count":1`, `"phase":"storage_body","count":1`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %s: %s", want, logs.String())
		}
	}
	for _, secret := range []string{"private-bucket", "recordings/packages/private", server.URL} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("storage details leaked")
		}
	}
}
