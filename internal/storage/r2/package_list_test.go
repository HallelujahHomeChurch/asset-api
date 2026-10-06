package r2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func packageListStore(server *httptest.Server) *Store {
	return &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
}

func TestListPackageObjectsUsesScopedProviderPagesNotHeadRequests(t *testing.T) {
	calls := 0
	prefix := "recordings/packages/package-a/staging/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/test" || r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != prefix || r.URL.Query().Get("max-keys") != "1000" {
			t.Errorf("unscoped listing request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Query().Get("continuation-token") {
		case "":
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>second-page</NextContinuationToken><Contents><Key>%s720p/init.mp4</Key><Size>100</Size></Contents></ListBucketResult>`, prefix)
		case "second-page":
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%smaster.m3u8</Key><Size>123</Size></Contents></ListBucketResult>`, prefix)
		default:
			t.Error("unexpected continuation")
		}
	}))
	defer server.Close()
	store := packageListStore(server)
	for _, id := range []string{"", "../another", "package-a/final/attempt", "package-a?prefix=all"} {
		if _, err := store.ListPackageObjects(context.Background(), id, 10_000); err == nil {
			t.Fatalf("invalid package sent: %q", id)
		}
	}
	for _, limit := range []int{0, 10_001} {
		if _, err := store.ListPackageObjects(context.Background(), "package-a", limit); err == nil {
			t.Fatal("unbounded listing accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached provider")
	}
	objects, err := store.ListPackageObjects(context.Background(), "package-a", 10_000)
	if err != nil || calls != 2 || len(objects) != 2 || objects["720p/init.mp4"] != 100 || objects["master.m3u8"] != 123 {
		t.Fatalf("listing: objects=%v calls=%d error=%v", objects, calls, err)
	}
}

func TestListPackageObjectsFailsClosedOnInvalidOrIncompleteProviderEvidence(t *testing.T) {
	prefix := "recordings/packages/package-a/staging/"
	content := func(path, size string) string {
		return "<Contents><Key>" + prefix + path + "</Key>" + size + "</Contents>"
	}
	first := content("720p/init.mp4", "<Size>100</Size>")
	for _, tc := range []struct {
		name, response string
		limit          int
	}{
		{"sibling prefix", strings.Replace(first, "package-a", "package-b", 1), 10_000},
		{"final prefix", strings.Replace(first, "/staging/", "/final/attempt/", 1), 10_000},
		{"invalid path", content("../other", "<Size>1</Size>"), 10_000},
		{"missing size", content("720p/init.mp4", ""), 10_000},
		{"negative size", content("720p/init.mp4", "<Size>-1</Size>"), 10_000},
		{"duplicate object", first + first, 10_000},
		{"object budget", first + content("master.m3u8", "<Size>123</Size>"), 1},
		{"missing continuation", "<IsTruncated>true</IsTruncated>" + first, 10_000},
		{"empty truncated page", "<IsTruncated>true</IsTruncated><NextContinuationToken>again</NextContinuationToken>", 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				response := tc.response
				if !strings.Contains(response, "<IsTruncated>") {
					response = "<IsTruncated>false</IsTruncated>" + response
				}
				fmt.Fprint(w, "<ListBucketResult>"+response+"</ListBucketResult>")
			}))
			defer server.Close()
			objects, err := packageListStore(server).ListPackageObjects(context.Background(), "package-a", tc.limit)
			if err == nil || objects != nil {
				t.Fatalf("invalid evidence returned: %v %v", objects, err)
			}
		})
	}
	for _, status := range []int{403, 503} {
		t.Run(fmt.Sprintf("provider status %d", status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			if objects, err := packageListStore(server).ListPackageObjects(context.Background(), "package-a", 10_000); err == nil || objects != nil {
				t.Fatal("provider error inferred missing objects")
			}
		})
	}
}

func TestListPackageObjectsRejectsMissingCompletionFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<ListBucketResult/>`)
	}))
	defer server.Close()
	if objects, err := packageListStore(server).ListPackageObjects(context.Background(), "package-a", 10_000); err == nil || objects != nil {
		t.Fatal("incomplete listing inferred missing objects")
	}
}

func TestListPackageObjectsRejectsContinuationCycles(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>same-token</NextContinuationToken><Contents><Key>recordings/packages/package-a/staging/720p/seg-%06d.m4s</Key><Size>100</Size></Contents></ListBucketResult>`, calls)
	}))
	defer server.Close()
	if objects, err := packageListStore(server).ListPackageObjects(context.Background(), "package-a", 10_000); err == nil || objects != nil || calls != 2 {
		t.Fatalf("provider loop: objects=%v calls=%d error=%v", objects, calls, err)
	}
}
