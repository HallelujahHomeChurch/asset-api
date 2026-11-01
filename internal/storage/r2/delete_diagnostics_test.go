package r2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestDeleteReportsCanceledAttemptWithoutObjectIdentifiers(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	store := &Store{bucket: "private-bucket", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := store.DeletePackageObjects(ctx, []string{"recordings/packages/private-id/staging/master.m3u8"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation lost: %v", err)
	}
	for _, want := range []string{`"msg":"recording_delete_attempt"`, `"objects":1`, `"attempt":1`, `"status_code":0`, `"deadline_exceeded":true`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing %s in %s", want, logs.String())
		}
	}
	for _, secret := range []string{"private-id", "private-bucket", server.URL, "master.m3u8"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("sensitive diagnostic %q", secret)
		}
	}
}

func TestDeleteReportsPartialFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<DeleteResult><Error><Key>private-key</Key><Code>AccessDenied</Code></Error></DeleteResult>`)
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	if err := store.DeleteCoverObjects(context.Background(), []string{"recordings/covers/a/b/input"}); err == nil {
		t.Fatal("partial failure accepted")
	}
	for _, want := range []string{`"status_code":200`, `"partial_errors":1`, `"failed":true`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing %s in %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "private-key") {
		t.Fatal("provider object key leaked")
	}
}

func TestDeleteReportsSDKRetriesAndSuccess(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/xml")
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `<Error><Code>ServiceUnavailable</Code></Error>`)
			return
		}
		fmt.Fprint(w, `<DeleteResult/>`)
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), Retryer: retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2; o.MaxBackoff = time.Millisecond })})}
	if err := store.DeletePackageObjects(context.Background(), []string{"recordings/packages/a/staging/master.m3u8"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("retry behavior changed: %d", calls)
	}
	for _, want := range []string{`"status_code":503`, `"status_code":200`, `"attempts":2`, `"partial_errors":0,"failed":false`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing %s in %s", want, logs.String())
		}
	}
}
