package r2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestPreviewWritesBoundedImmutableAndNotClientSignable(t *testing.T) {
	calls := 0
	conflict := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" || r.Header.Get("Cache-Control") != "private, no-store" {
			t.Error("unsafe preview write")
		}
		if conflict {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(412)
			fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		}
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	prefix := "recordings/packages/package/final/validated/previews/"
	for _, key := range []string{"recordings/packages/package/staging/previews/index.vtt", prefix + "../index.vtt", prefix + "index.vtt", prefix + "attempt/other.jpg"} {
		if err := store.PutPackagePreview(context.Background(), key, []byte("WEBVTT\n")); err == nil {
			t.Fatal("bad key allowed")
		}
	}
	if err := store.PutPackagePreview(context.Background(), prefix+"attempt/seg-000000.jpg", make([]byte, (1<<20)+1)); err == nil {
		t.Fatal("oversize allowed")
	}
	if calls != 0 {
		t.Fatal("invalid write reached storage")
	}
	for _, key := range []string{prefix + "attempt/index.vtt", prefix + "attempt/seg-000000.jpg"} {
		if _, err := store.PresignPackageObject(context.Background(), key, 10, "video/mp4", time.Minute); err == nil {
			t.Fatal("preview client signable")
		}
	}
	if err := store.PutPackagePreview(context.Background(), prefix+"attempt/index.vtt", []byte("WEBVTT\n\n")); err != nil {
		t.Fatal(err)
	}
	conflict = true
	if err := store.PutPackagePreview(context.Background(), prefix+"current.json", []byte(`{"attempt":"winner"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPackagePreview(context.Background(), prefix+"attempt/seg-000000.jpg", []byte("jpeg")); err == nil {
		t.Fatal("sprite overwrite accepted")
	}
	if err := store.PutPackagePreview(context.Background(), prefix+"current.json", []byte(`{"attempt":"`+strings.Repeat("x", 81)+`"}`)); err == nil {
		t.Fatal("oversize attempt allowed")
	}
}
