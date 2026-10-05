package r2

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestCoverWritesPrivateImmutableAndBounded(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" || r.Header.Get("Cache-Control") != "private, no-store" || r.Header.Get("Content-Type") != "image/jpeg" {
			t.Error("unsafe cover write")
		}
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	var imageBytes bytes.Buffer
	if err := jpeg.Encode(&imageBytes, image.NewGray(image.Rect(0, 0, 1280, 720)), &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"recordings/packages/id/master.m3u8", "recordings/covers/../attempt/custom.jpg", "recordings/covers/r/a/auto-4.jpg", "recordings/covers/r/a/original.jpg"} {
		if err := store.PutRecordingCover(ctx, key, imageBytes.Bytes()); err == nil {
			t.Fatalf("invalid key: %s", key)
		}
	}
	key := "recordings/covers/recording/attempt/custom.jpg"
	for _, data := range [][]byte{nil, []byte("not an image"), make([]byte, (1<<20)+1)} {
		if err := store.PutRecordingCover(ctx, key, data); err == nil {
			t.Fatal("invalid bytes accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid input reached R2")
	}
	if _, err := store.PresignPackageObject(ctx, key, 100, "video/mp4", time.Minute); err == nil {
		t.Fatal("cover may be overwritten by upload signer")
	}
	if err := store.PutRecordingCover(ctx, key, imageBytes.Bytes()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("puts %d", calls)
	}
}

func TestCoverInputRejectsInvalidScopeBeforeProvider(t *testing.T) {
	store := &Store{}
	if err := store.PutCoverInput(context.Background(), "recordings/packages/p/input", []byte("x"), "image/png"); err == nil {
		t.Fatal("wrong namespace")
	}
	if err := store.DeleteCoverObjects(context.Background(), []string{"recordings/covers/a/x/input", "recordings/covers/b/y/input"}); err == nil {
		t.Fatal("mixed recording delete")
	}
}
