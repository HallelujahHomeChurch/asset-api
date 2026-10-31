package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestPackageCleanupListsPurgedAttemptsOncePerInvocation(t *testing.T) {
	lists, deletes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == "GET" {
			lists++
			if r.URL.Query().Get("prefix") != "recordings/packages/a/final/" {
				t.Error("wrong listing scope")
			}
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		deletes++
		fmt.Fprint(w, `<DeleteResult/>`)
	}))
	defer server.Close()
	store := cleanupTestStore(server)
	remove := store.NewPackageCleanup()
	for i := 0; i < 100; i++ {
		if err := remove(context.Background(), []string{fmt.Sprintf("recordings/packages/a/final/attempt-%d/480p/init.mp4", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if lists != 1 || deletes != 0 {
		t.Fatalf("empty retained attempts still cause provider calls: lists=%d deletes=%d", lists, deletes)
	}
}

func TestPackageCleanupPreservesDeclaredScopeAndFreshLateWriteSweep(t *testing.T) {
	const key = "recordings/packages/a/staging/master.m3u8"
	lists, deletes := 0, 0
	present := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == "GET" {
			lists++
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
			if present {
				fmt.Fprintf(w, `<Contents><Key>%s</Key></Contents><Contents><Key>recordings/packages/a/staging/undeclared.txt</Key></Contents>`, key)
			}
			fmt.Fprint(w, `</ListBucketResult>`)
			return
		}
		deletes++
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), key) || strings.Contains(string(b), "undeclared") {
			t.Error("deletion escaped declared intersection")
		}
		fmt.Fprint(w, `<DeleteResult/>`)
	}))
	defer server.Close()
	store := cleanupTestStore(server)
	first := store.NewPackageCleanup()
	if err := first(context.Background(), []string{key}); err != nil {
		t.Fatal(err)
	}
	present = true
	if err := first(context.Background(), []string{key}); err != nil {
		t.Fatal(err)
	}
	if lists != 1 || deletes != 0 {
		t.Fatal("snapshot unexpectedly reused a future write")
	}
	second := store.NewPackageCleanup()
	if err := second(context.Background(), []string{key}); err != nil {
		t.Fatal(err)
	}
	if lists != 2 || deletes != 1 {
		t.Fatalf("late write not swept: lists=%d deletes=%d", lists, deletes)
	}
	if err := second(context.Background(), []string{key}); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatal("successfully removed key deleted again")
	}
}

func TestPackageCleanupFailsClosedOnInvalidScopeAndEscapedList(t *testing.T) {
	calls, deletes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			deletes++
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>recordings/packages/other/staging/master.m3u8</Key></Contents></ListBucketResult>`)
	}))
	defer server.Close()
	remove := cleanupTestStore(server).NewPackageCleanup()
	if err := remove(context.Background(), []string{"recordings/packages/a/staging/master.m3u8", "recordings/packages/b/staging/master.m3u8"}); err == nil || calls != 0 {
		t.Fatal("mixed scope reached provider")
	}
	if err := remove(context.Background(), []string{"recordings/packages/a/staging/master.m3u8"}); err == nil || deletes != 0 {
		t.Fatal("escaped listing accepted")
	}
}

func cleanupTestStore(server *httptest.Server) *Store {
	return &Store{bucket: "test", client: s3.New(s3.Options{BaseEndpoint: aws.String(server.URL), Region: "auto", UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: server.Client(), RetryMaxAttempts: 1})}
}

func TestPackageCleanupPaginationAndPartialFailureRemainRetryable(t *testing.T) {
	const key = "recordings/packages/a/staging/master.m3u8"
	lists, deletes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == "GET" {
			lists++
			if r.URL.Query().Get("continuation-token") == "" {
				fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken></ListBucketResult>`)
			} else {
				fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key></Contents></ListBucketResult>`, key)
			}
			return
		}
		deletes++
		if deletes == 1 {
			fmt.Fprintf(w, `<DeleteResult><Error><Key>%s</Key><Code>InternalError</Code></Error></DeleteResult>`, key)
		} else {
			fmt.Fprint(w, `<DeleteResult/>`)
		}
	}))
	defer server.Close()
	remove := cleanupTestStore(server).NewPackageCleanup()
	if err := remove(context.Background(), []string{key}); err == nil {
		t.Fatal("partial deletion acknowledged")
	}
	if err := remove(context.Background(), []string{key}); err != nil {
		t.Fatal(err)
	}
	if lists != 2 || deletes != 2 {
		t.Fatalf("failed keys not retained: lists=%d deletes=%d", lists, deletes)
	}
}

func TestPackageCleanupFailedListingIsNotCached(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Error("delete after list failure")
		}
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()
	remove := cleanupTestStore(server).NewPackageCleanup()
	keys := []string{"recordings/packages/a/staging/master.m3u8"}
	if err := remove(context.Background(), keys); err == nil {
		t.Fatal("list error accepted")
	}
	if err := remove(context.Background(), keys); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("failed list cached")
	}
}

func TestPackageCleanupListingIsBoundedAndCancellationIsPreserved(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>page-%d</NextContinuationToken></ListBucketResult>`, calls)
	}))
	defer server.Close()
	remove := cleanupTestStore(server).NewPackageCleanup()
	keys := []string{"recordings/packages/a/staging/master.m3u8"}
	if err := remove(context.Background(), keys); err == nil || calls != 100 {
		t.Fatalf("listing escaped bound: calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := remove(ctx, keys); !errors.Is(err, context.Canceled) || calls != 100 {
		t.Fatal("canceled cleanup reached provider")
	}
}
