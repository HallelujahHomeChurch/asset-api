package r2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestDeleteSourceAttemptRejectsEscapedListing(t *testing.T) {
	id := strings.Repeat("a", 32)
	bad, deletes := true, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == "GET" {
			prefix := r.URL.Query().Get("prefix")
			if prefix != "recordings/packages/"+id+"/staging/" && prefix != "recordings/packages/"+id+"/final/"+id+"/" {
				t.Errorf("unbounded list %s", prefix)
			}
			key := prefix + "720p/init.mp4"
			if bad {
				key = "recordings/packages/other/staging/720p/init.mp4"
			}
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key></Contents></ListBucketResult>`, key)
			return
		}
		if r.Method != "POST" || !r.URL.Query().Has("delete") {
			t.Errorf("unexpected request")
		}
		deletes++
		fmt.Fprint(w, `<DeleteResult/>`)
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	if err := store.DeleteSourceAttempt(context.Background(), id); err == nil || deletes != 0 {
		t.Fatal("escaped listing deleted")
	}
	bad = false
	if err := store.DeleteSourceAttempt(context.Background(), id); err != nil || deletes != 2 {
		t.Fatalf("cleanup: %v deletes=%d", err, deletes)
	}
}

func TestDeletePackageObjectsRejectsMixedScopeAndProviderPartialFailure(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !r.URL.Query().Has("delete") || !strings.Contains(string(body), "recordings/packages/a/staging/master.m3u8") {
			t.Errorf("delete request: %s %s %s", r.Method, r.URL, body)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<DeleteResult><Error><Key>recordings/packages/a/staging/master.m3u8</Key><Code>AccessDenied</Code></Error></DeleteResult>`))
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	for _, keys := range [][]string{nil, {"recordings/other.mp4"}, {"recordings/packages/a/staging/master.m3u8", "recordings/packages/b/staging/master.m3u8"}, {"recordings/packages/a/final/attempt/master.m3u8", "recordings/packages/a/final/other/master.m3u8"}} {
		if err := store.DeletePackageObjects(context.Background(), keys); err == nil {
			t.Fatalf("unsafe delete accepted %v", keys)
		}
	}
	if requests != 0 {
		t.Fatal("unsafe request sent")
	}
	if err := store.DeletePackageObjects(context.Background(), []string{"recordings/packages/a/staging/master.m3u8"}); err == nil {
		t.Fatal("partial provider failure accepted")
	}
}

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
		HTTPClient:  server.Client(), RetryMaxAttempts: 1,
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

func TestRecordingSpoolWriteIsBoundedAndNeverOverwrites(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" || r.ContentLength != 5 || r.Header.Get("Content-Type") != "video/mp4" || string(data) != "media" {
			t.Error("incorrect bounded conditional PUT")
		}
		if r.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "" {
			t.Error("unsupported automatic checksum")
		}
		w.Header().Set("ETag", `"stored"`)
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired})}
	key := "recordings/packages/attempt-a/staging/720p/seg-000000.m4s"
	for _, bad := range []string{"ordinary/blob", strings.Replace(key, "/staging/", "/final/claim/", 1)} {
		if err := store.PutRecordingObject(context.Background(), bad, strings.NewReader("media"), 5, "video/mp4"); err == nil {
			t.Fatal("accepted wrong scope")
		}
	}
	if err := store.PutRecordingObject(context.Background(), key, strings.NewReader("media"), 129<<20, "video/mp4"); err == nil {
		t.Fatal("accepted oversized object")
	}
	if calls != 0 {
		t.Fatal("invalid request reached R2")
	}
	if err := store.PutRecordingObject(context.Background(), key, strings.NewReader("media"), 5, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unexpected PUT count")
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

func TestPresignPackageOnlyWritesOneSizedStagingObject(t *testing.T) {
	store, err := New("account-id", "recordings-test", "test-key", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	key := "recordings/packages/package-a/staging/720p/seg-000001.m4s"
	signed, err := store.PresignPackageObject(context.Background(), key, 123, "video/mp4", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed.URL)
	if signed.Method != "PUT" || u.Path != "/recordings-test/"+key || u.Query().Get("X-Amz-Expires") != "900" {
		t.Fatalf("wrong scope: %s", u.Redacted())
	}
	if http.Header(signed.Headers).Get("Content-Length") != "123" {
		t.Fatalf("missing fixed length: %v", signed.Headers)
	}
	if !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "content-length") || !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "content-type") {
		t.Fatal("size and content type must participate in the signature")
	}
	for _, bad := range []string{"recordings/packages/package-a/final/720p/init.mp4", "recordings/packages/package-a/staging/../secret", "recordings/packages/package-a/staging/package.json", "recordings/packages/package-a/staging/720p/init.mp4?x=1", "recordings/packages/package-a/staging/720p/%2e%2e"} {
		if _, err := store.PresignPackageObject(context.Background(), bad, 123, "video/mp4", time.Minute); err == nil {
			t.Fatalf("signed forbidden key: %q", bad)
		}
	}
	for _, size := range []int64{0, 128<<20 + 1} {
		if _, err := store.PresignPackageObject(context.Background(), key, size, "video/mp4", time.Minute); err == nil {
			t.Fatalf("signed invalid size: %d", size)
		}
	}
	if _, err := store.PresignPackageObject(context.Background(), key, 123, "video/mp4", 16*time.Minute); err == nil {
		t.Fatal("signed overlong URL")
	}
}

func TestPackageCopyPinsSourceETagAndCannotCrossPackage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "PUT" || r.URL.Path != "/recordings-test/recordings/packages/package-a/final/attempt-a/720p/init.mp4" || r.Header.Get("X-Amz-Copy-Source-If-Match") != `"source-etag"` {
			t.Errorf("incorrect copy scope: %s %s", r.Method, r.URL.Path)
		}
		source, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
		if err != nil || source != "recordings-test/recordings/packages/package-a/staging/720p/init.mp4" {
			t.Errorf("copy source=%q", source)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<CopyObjectResult><ETag>"final-etag"</ETag><LastModified>2026-09-30T00:00:00Z</LastModified></CopyObjectResult>`))
	}))
	defer server.Close()
	store := &Store{bucket: "recordings-test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	from := "recordings/packages/package-a/staging/720p/init.mp4"
	to := "recordings/packages/package-a/final/attempt-a/720p/init.mp4"
	if err := store.CopyPackageObject(context.Background(), from, to, `"source-etag"`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(to, "package-a", "package-b", 1), strings.Replace(to, "init.mp4", "seg-000000.m4s", 1), from} {
		if err := store.CopyPackageObject(context.Background(), from, bad, `"source-etag"`); err == nil {
			t.Fatalf("invalid copy destination accepted: %s", bad)
		}
	}
	if err := store.CopyPackageObject(context.Background(), from, to, ""); err == nil {
		t.Fatal("copy without conditional source")
	}
	if requests != 1 {
		t.Fatalf("invalid requests reached provider: %d", requests)
	}
}

func TestPackageControlInventoryOnlyUsesServerFinalPath(t *testing.T) {
	requests := 0
	data := []byte(`{"schemaVersion":1}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "PUT" || r.URL.Path != "/recordings-test/recordings/packages/package-a/final/attempt-a/package.json" || r.ContentLength != int64(len(data)) || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("invalid control PUT: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(200)
	}))
	defer server.Close()
	store := &Store{bucket: "recordings-test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	if err := store.PutPackageInventory(context.Background(), "recordings/packages/package-a/final/attempt-a/package.json", data); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPackageInventory(context.Background(), "recordings/packages/package-a/staging/package.json", data); err == nil {
		t.Fatal("staging inventory PUT accepted")
	}
	if err := store.PutPackageInventory(context.Background(), "recordings/packages/package-a/final/attempt-a/package.json", []byte("invalid")); err == nil {
		t.Fatal("invalid control JSON accepted")
	}
	if requests != 1 {
		t.Fatal("invalid control PUT reached provider")
	}
}
