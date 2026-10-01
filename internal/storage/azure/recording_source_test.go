package azure

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"hhc/asset-api/internal/assets"
)

func TestDeleteSourceOnlyRemovesKnownSourceAndAttempts(t *testing.T) {
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Fatal("non-delete cleanup")
		}
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "source-test"}
	id, attempt := strings.Repeat("a", 32), strings.Repeat("b", 32)
	if err := store.DeleteRecordingSource(context.Background(), id, []string{attempt, "../asset"}); err == nil || len(paths) != 0 {
		t.Fatal("invalid deletion partially executed")
	}
	if err := store.DeleteRecordingSource(context.Background(), id, []string{attempt}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/source-test/recording-sources/"+id+"/staging" || paths[1] != "/source-test/recording-sources/"+id+"/final/"+attempt+"/source" {
		t.Fatalf("wrong keys: %v", paths)
	}
}

func TestSourceBlockSASIsWriteOnlySingleBlobAndShortLived(t *testing.T) {
	keys := 0
	now := time.Now().UTC()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys++
		if r.URL.Query().Get("comp") != "userdelegationkey" {
			t.Error("unexpected provider call")
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<UserDelegationKey><SignedOid>11111111-1111-4111-8111-111111111111</SignedOid><SignedTid>22222222-2222-4222-8222-222222222222</SignedTid><SignedStart>%s</SignedStart><SignedExpiry>%s</SignedExpiry><SignedService>b</SignedService><SignedVersion>2025-11-05</SignedVersion><Value>dGVzdA==</Value></UserDelegationKey>`, now.Add(-time.Minute).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "recording-source-fixture"}
	id := strings.Repeat("a", 32)
	target, err := store.SignRecordingSourceBlock(context.Background(), id, 2981, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := assets.RecordingSourceBlockID(2981)
	if u.Scheme != "https" || u.Path != "/recording-source-fixture/recording-sources/"+id+"/staging" || u.Query().Get("sp") != "w" || u.Query().Get("sr") != "b" || u.Query().Get("spr") != "https" || u.Query().Get("comp") != "block" || u.Query().Get("blockid") != block || target.Method != "PUT" {
		t.Fatal("incorrect source capability scope")
	}
	for _, bad := range []string{"../ordinary-asset", id + "/final", ""} {
		if _, err := store.SignRecordingSourceBlock(context.Background(), bad, 1, now.Add(time.Minute)); err == nil {
			t.Fatal("accepted arbitrary object key")
		}
	}
	if _, err := store.SignRecordingSourceBlock(context.Background(), id, 1, now.Add(time.Hour)); err == nil {
		t.Fatal("accepted excessive capability lifetime")
	}
	if keys != 1 {
		t.Fatal("invalid requests reached credential provider")
	}
}

func TestSourceCopyResumesPendingWithoutRewritingImmutableDestination(t *testing.T) {
	id, attempt := strings.Repeat("a", 32), strings.Repeat("b", 32)
	source := assets.RecordingSource{ID: id, SizeBytes: 5, ChecksumSHA256: strings.Repeat("c", 64), StagingETag: `"staging-v2"`}
	storedETagHash := fmt.Sprintf("%x", sha256.Sum256([]byte(source.StagingETag)))
	created, copies, state := false, 0, "pending"
	target := "/source-fixture/recording-sources/" + id + "/final/" + attempt + "/source"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != target {
			t.Error("unexpected source/destination path")
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "HEAD":
			if !created {
				w.Header().Set("x-ms-error-code", "BlobNotFound")
				w.WriteHeader(404)
				return
			}
			w.Header().Set("x-ms-copy-id", "copy-a")
			w.Header().Set("x-ms-copy-status", state)
			w.Header().Set("ETag", `"immutable-v3"`)
			w.Header().Set("Content-Length", "5")
			w.Header().Set("x-ms-meta-hhc_source", id)
			w.Header().Set("x-ms-meta-hhc_etag", storedETagHash)
			w.Header().Set("x-ms-meta-hhc_sha256", source.ChecksumSHA256)
		case "PUT":
			copies++
			created = true
			from, err := url.Parse(r.Header.Get("x-ms-copy-source"))
			if err != nil || from.Path != "/source-fixture/recording-sources/"+id+"/staging" || from.RawQuery != "" || r.Header.Get("x-ms-source-if-match") != source.StagingETag || r.Header.Get("If-None-Match") != "*" {
				t.Error("copy must fence source and never replace destination or use public SAS")
			}
			if r.Header.Get("x-ms-meta-hhc_source") != id || r.Header.Get("x-ms-meta-hhc_sha256") != source.ChecksumSHA256 {
				t.Error("copy identity missing")
			}
			w.Header().Set("x-ms-copy-id", "copy-a")
			w.Header().Set("x-ms-copy-status", "pending")
			w.WriteHeader(202)
		default:
			t.Error("copy API must not download source bytes")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "source-fixture"}
	for i := 0; i < 2; i++ {
		copy, err := store.CopyRecordingSource(context.Background(), source, attempt)
		if err != nil || copy.State != "pending" || copy.CopyID != "copy-a" {
			t.Fatalf("pending copy: %+v %v", copy, err)
		}
	}
	state = "success"
	copy, err := store.CopyRecordingSource(context.Background(), source, attempt)
	if err != nil || copy.State != "success" || copy.ETag != `"immutable-v3"` || copy.SizeBytes != 5 || copies != 1 {
		t.Fatalf("recovered copy %+v %v calls=%d", copy, err, copies)
	}
	source.StagingETag = `"changed"`
	if _, err := store.CopyRecordingSource(context.Background(), source, attempt); err == nil {
		t.Fatal("reused destination for changed source version")
	}
}

func TestSourceBlockCommitPinsETagAndNeverReadsBodyInRequest(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			id := strings.Repeat("b", 32)
			block, _ := assets.RecordingSourceBlockID(1)
			commits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Query().Get("comp") == "blocklist":
					w.Header().Set("Content-Type", "application/xml")
					w.Header().Set("ETag", `"staging-v1"`)
					fmt.Fprintf(w, `<BlockList><CommittedBlocks/><UncommittedBlocks><Block><Name>%s</Name><Size>5</Size></Block></UncommittedBlocks></BlockList>`, block)
				case r.Method == "PUT" && r.URL.Query().Get("comp") == "blocklist":
					commits++
					body, _ := io.ReadAll(r.Body)
					if r.Header.Get("If-Match") != `"staging-v1"` || !strings.Contains(string(body), "<Latest>"+block+"</Latest>") {
						t.Error("unfenced or unordered commit")
					}
					w.Header().Set("ETag", `"committed-v2"`)
					w.WriteHeader(201)
				case r.Method == "HEAD":
					etag := `"committed-v2"`
					if changed {
						etag = `"changed-v3"`
					}
					w.Header().Set("ETag", etag)
					w.Header().Set("Content-Length", "5")
				default:
					t.Errorf("body read/copy or unexpected provider request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			client, err := azblob.NewClientWithNoCredential(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			store := &Store{client: client, container: "source-fixture"}
			value, err := store.CommitRecordingSource(context.Background(), id, 5)
			if commits != 1 || changed && err == nil || !changed && (err != nil || value.Size != 5 || value.ETag != `"committed-v2"`) {
				t.Fatalf("commit changed=%v metadata=%+v err=%v", changed, value, err)
			}
		})
	}
}

func TestSourceBlockListIsNotTruncatedAtOneThousand(t *testing.T) {
	count, _ := assets.RecordingSourceBlockCount(assets.RecordingSourceMaxBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("blocklisttype") != "all" {
			t.Error("incorrect provider listing")
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, "<BlockList><CommittedBlocks/><UncommittedBlocks>")
		for n := 1; n <= count; n++ {
			id, _ := assets.RecordingSourceBlockID(n)
			size := assets.RecordingSourceBlockBytes
			if n == count {
				size = assets.RecordingSourceMaxBytes - int64(count-1)*assets.RecordingSourceBlockBytes
			}
			fmt.Fprintf(w, "<Block><Name>%s</Name><Size>%d</Size></Block>", id, size)
		}
		fmt.Fprint(w, "</UncommittedBlocks></BlockList>")
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "source-fixture"}
	value, err := store.RecordingSourceBlocks(context.Background(), strings.Repeat("a", 32))
	if err != nil || len(value.Uncommitted) != 2981 {
		t.Fatalf("truncated provider list: %d %v", len(value.Uncommitted), err)
	}
	if _, err := assets.ValidateRecordingSourceBlocks(assets.RecordingSourceMaxBytes, value.Committed, value.Uncommitted); err != nil {
		t.Fatal(err)
	}
}
