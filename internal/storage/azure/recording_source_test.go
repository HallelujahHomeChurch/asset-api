package azure

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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"hhc/asset-api/internal/assets"
)

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
