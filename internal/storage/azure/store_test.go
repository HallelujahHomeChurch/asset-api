package azure

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"hhc/asset-api/internal/assets"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func TestContentRangeTotal(t *testing.T) {
	if total := contentRangeTotal("bytes 10-19/100", 10); total != 100 {
		t.Fatalf("total = %d", total)
	}
	if total := contentRangeTotal("", 10); total != 10 {
		t.Fatalf("fallback total = %d", total)
	}
}

func TestMapErrorClassifiesBlobPreconditions(t *testing.T) {
	for status, expected := range map[int]error{
		http.StatusNotFound:                     assets.ErrNotFound,
		http.StatusConflict:                     assets.ErrConflict,
		http.StatusPreconditionFailed:           assets.ErrInvalidUpload,
		http.StatusRequestedRangeNotSatisfiable: assets.ErrInvalidInput,
	} {
		if err := mapError(&azcore.ResponseError{StatusCode: status}); !errors.Is(err, expected) {
			t.Fatalf("status %d mapped to %v", status, err)
		}
	}
}

func TestDeleteMissingBlobIsRepeatSafe(t *testing.T) {
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method=%s", r.Method)
		}
		deletes++
		w.Header().Set("x-ms-error-code", "BlobNotFound")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "fixture"}
	for range 2 {
		if err := store.Delete(context.Background(), "missing"); err != nil {
			t.Fatal(err)
		}
	}
	if deletes != 2 {
		t.Fatalf("deletes=%d", deletes)
	}
}
