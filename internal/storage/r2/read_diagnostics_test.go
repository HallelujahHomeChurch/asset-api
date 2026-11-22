package r2

import (
	"bytes"
	"context"
	"errors"
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
	"sync/atomic"
	"testing"
	"time"
)

func TestR2DownloadRecordsRealSDKRetriesAndBody(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	var attempts atomic.Int64
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
		body, err := store.Open(WithAttemptCounter(r.Context(), &attempts), "recordings/packages/private/master.m3u8")
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
	if calls != 2 || attempts.Load() != 2 {
		t.Fatal("SDK retry changed", calls)
	}
	for _, want := range []string{`"storage_attempt":{"count":2`, `"storage_retry":{"count":1`, `"storage_body":{"count":1`} {
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

// A HEAD deadline must survive SDK wrapping so the live publisher can tell
// it from a provider status. Count actual sends when the SDK retries a 503.
func TestR2HeadDeadlineAndAttemptAccounting(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Errorf("unexpected method %s", r.Method)
		}
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			<-r.Context().Done()
		default:
			w.Header().Set("Content-Length", "10")
			w.Header().Set("ETag", `"version"`)
		}
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), Retryer: retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2; o.MaxBackoff = time.Millisecond })})}
	var sends atomic.Int64
	ctx, cancel := context.WithTimeout(WithAttemptCounter(t.Context(), &sends), time.Second)
	defer cancel()
	const key = "recordings/packages/test/final/claim/720p/seg-000000.m4s"
	_, _, err := store.Head(ctx, key)
	if !errors.Is(err, context.DeadlineExceeded) || sends.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("err=%v sends=%d calls=%d", err, sends.Load(), calls.Load())
	}
	size, etag, err := store.Head(WithAttemptCounter(t.Context(), &sends), key)
	if err != nil || size != 10 || etag != `"version"` || sends.Load() != 3 {
		t.Fatalf("size=%d etag=%q err=%v sends=%d", size, etag, err, sends.Load())
	}
}
