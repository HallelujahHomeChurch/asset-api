package azure

import (
	"bytes"
	"context"
	"errors"
	"io"
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

func TestPersonalPutOnceUsesAtomicPrecondition(t *testing.T) {
	committed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Query().Get("comp") == "block" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		committed = true
		if r.Header.Get("If-None-Match") != "*" {
			t.Errorf("missing atomic precondition: %v", r.Header)
		}
		w.Header().Set("x-ms-error-code", "ConditionNotMet")
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "private"}
	_, err = store.PutOnce(context.Background(), "personal/staging", bytes.NewBufferString("immutable"), 9, "application/pdf")
	if !committed || !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("commit=%v err=%v", committed, err)
	}
}
