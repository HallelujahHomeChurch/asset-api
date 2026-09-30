package r2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestAbortAlreadyMissingMultipartUploadIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Query().Get("uploadId") != "upload-1" {
			t.Errorf("unexpected abort request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchUpload</Code><Message>already gone</Message></Error>`))
	}))
	defer server.Close()
	store := &Store{bucket: "recordings-test", client: s3.New(s3.Options{
		BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""),
		HTTPClient: server.Client(), RetryMaxAttempts: 1,
	})}
	if err := store.Abort(context.Background(), "recordings/version.mp4", "upload-1"); err != nil {
		t.Fatalf("missing multipart upload must count as aborted: %v", err)
	}
}

func TestR2DoesNotSendUnsupportedAutomaticChecksumHeaders(t *testing.T) {
	store, err := New("account-id", "recordings-test", "test-key", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if store.client.Options().RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Fatal("R2 adapter must not enable optional AWS checksum headers")
	}
}

func TestPresignPartIsLimitedToOneUploadAndFifteenMinutes(t *testing.T) {
	store, err := New("account-id", "recordings-test", "test-key", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	signed, err := store.PresignPart(context.Background(), "recordings/file-1.mp4", "upload-1", 597, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "account-id.r2.cloudflarestorage.com" || parsed.Path != "/recordings-test/recordings/file-1.mp4" {
		t.Fatalf("unexpected R2 target: %s", parsed.Redacted())
	}
	query := parsed.Query()
	if query.Get("uploadId") != "upload-1" || query.Get("partNumber") != "597" || query.Get("X-Amz-Expires") != "900" {
		t.Fatalf("unexpected constrained part URL: %s", parsed.Redacted())
	}
	if query.Has("x-amz-sdk-checksum-algorithm") || query.Has("x-amz-checksum-algorithm") {
		t.Fatal("R2 does not support automatic AWS checksum headers")
	}
	if signed.Method != "PUT" {
		t.Fatalf("method = %q", signed.Method)
	}
	for _, part := range []int{0, 598} {
		if _, err := store.PresignPart(context.Background(), "recordings/file-1.mp4", "upload-1", part, time.Minute); err == nil {
			t.Errorf("part %d must be rejected", part)
		}
	}
	if _, err := store.PresignPart(context.Background(), "recordings/file-1.mp4", "upload-1", 1, 16*time.Minute); err == nil {
		t.Fatal("URL longer than 15 minutes must be rejected")
	}
}

func TestPresignProbeReadOnlyTargetsOneImmutableRecording(t *testing.T) {
	store, err := New("account-id", "recordings-test", "test-key", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	signed, err := store.PresignProbeRead(context.Background(), "recordings/file-1.mp4", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Method != "GET" || parsed.Path != "/recordings-test/recordings/file-1.mp4" || parsed.Query().Get("X-Amz-Expires") != "900" {
		t.Fatalf("unexpected probe URL: %s", parsed.Redacted())
	}
	if _, err := store.PresignProbeRead(context.Background(), "../other", time.Minute); err == nil {
		t.Fatal("non-recording key must fail")
	}
}
