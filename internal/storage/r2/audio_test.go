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

func TestAudioWritesBoundedImmutableAndNotClientSignable(t *testing.T) {
	calls := 0
	conflict := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" || r.Header.Get("Cache-Control") != "private, no-store" {
			t.Error("unsafe audio write")
		}
		if strings.HasSuffix(r.URL.Path, ".ts") && r.Header.Get("Content-Type") != "video/mp2t" {
			t.Error("incorrect audio transport MIME")
		}
		if conflict {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(412)
			fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		}
	}))
	defer server.Close()
	store := &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
	prefix := "recordings/packages/package/final/validated/audio/aac-hls-v1/"
	for _, key := range []string{"recordings/packages/package/staging/audio/aac-hls-v1/index.m3u8", prefix + "../index.m3u8", prefix + "index.m3u8", prefix + "attempt/other.ts"} {
		if err := store.PutPackageAudio(context.Background(), key, []byte("#EXTM3U\n")); err == nil {
			t.Fatal("bad key allowed")
		}
	}
	if err := store.PutPackageAudio(context.Background(), prefix+"attempt/seg-000000.ts", make([]byte, (8<<20)+1)); err == nil {
		t.Fatal("oversize allowed")
	}
	if calls != 0 {
		t.Fatal("invalid write reached storage")
	}
	for _, key := range []string{prefix + "attempt/index.m3u8", prefix + "attempt/seg-000000.ts"} {
		if _, err := store.PresignPackageObject(context.Background(), key, 10, "video/mp4", time.Minute); err == nil {
			t.Fatal("audio client signable")
		}
	}
	if err := store.PutPackageAudio(context.Background(), prefix+"attempt/index.m3u8", []byte("#EXTM3U\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPackageAudio(context.Background(), prefix+"attempt/seg-000000.ts", []byte("transport stream")); err != nil {
		t.Fatal(err)
	}
	conflict = true
	if err := store.PutPackageAudio(context.Background(), prefix+"current.json", []byte(`{"attempt":"winner"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPackageAudio(context.Background(), prefix+"attempt/seg-000000.ts", []byte("transport stream")); err == nil {
		t.Fatal("audio overwrite accepted")
	}
	if err := store.PutPackageAudio(context.Background(), prefix+"current.json", []byte(`{"attempt":"`+strings.Repeat("x", 81)+`"}`)); err == nil {
		t.Fatal("oversize attempt allowed")
	}
}
