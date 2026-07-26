package local

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignedUploadTargetWritesOnlyItsObject(t *testing.T) {
	store, err := New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateUploadTarget(context.Background(), "assets/cms.weekly.pdf/2026/07/asset-1/original", 1024, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, target.URL, bytes.NewBufferString("%PDF-1.7\ntest"))
	response := httptest.NewRecorder()
	request.SetPathValue("token", request.URL.Path[len("/dev/uploads/"):])
	store.PutHandler(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	properties, err := store.Inspect(context.Background(), "assets/cms.weekly.pdf/2026/07/asset-1/original")
	if err != nil {
		t.Fatal(err)
	}
	if properties.DetectedMIMEType != "application/pdf" {
		t.Fatalf("mime=%s", properties.DetectedMIMEType)
	}
}

func TestUploadTargetRejectsTamperingAndOversize(t *testing.T) {
	store, err := New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateUploadTarget(context.Background(), "assets/test/original", 4, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, target.URL+"x", bytes.NewBufferString("12345"))
	response := httptest.NewRecorder()
	request.SetPathValue("token", request.URL.Path[len("/dev/uploads/"):])
	store.PutHandler(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("tampered status=%d", response.Code)
	}

	validTarget, err := store.CreateUploadTarget(context.Background(), "assets/test/original", 4, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	oversized := httptest.NewRequest(http.MethodPut, validTarget.URL, bytes.NewBufferString("12345"))
	oversizedResponse := httptest.NewRecorder()
	oversized.SetPathValue("token", oversized.URL.Path[len("/dev/uploads/"):])
	store.PutHandler(oversizedResponse, oversized)
	if oversizedResponse.Code != http.StatusBadRequest {
		t.Fatalf("oversized status=%d", oversizedResponse.Code)
	}
}

func TestSignedUploadTargetCannotOverwriteObject(t *testing.T) {
	store, err := New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateUploadTarget(context.Background(), "assets/staging/session-1", 1024, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	put := func(value string) int {
		request := httptest.NewRequest(http.MethodPut, target.URL, bytes.NewBufferString(value))
		request.SetPathValue("token", request.URL.Path[len("/dev/uploads/"):])
		response := httptest.NewRecorder()
		store.PutHandler(response, request)
		return response.Code
	}
	if status := put("first"); status != http.StatusCreated {
		t.Fatalf("first status = %d", status)
	}
	if status := put("replacement"); status != http.StatusConflict {
		t.Fatalf("replacement status = %d", status)
	}
}

func TestCommitMovesStagingObjectWithoutReplacingFinal(t *testing.T) {
	store, err := New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	writeLocalUpload(t, store, "assets/staging/session-1", "%PDF-1.7\nfirst")
	properties, err := store.Commit(context.Background(), "assets/staging/session-1", "assets/final/asset-1/original")
	if err != nil {
		t.Fatal(err)
	}
	if properties.ETag == "" {
		t.Fatal("committed ETag is empty")
	}
	if _, err := store.Inspect(context.Background(), "assets/staging/session-1"); err == nil {
		t.Fatal("staging object still exists")
	}

	writeLocalUpload(t, store, "assets/staging/session-2", "%PDF-1.7\nreplacement")
	if _, err := store.Commit(context.Background(), "assets/staging/session-2", "assets/final/asset-1/original"); err == nil {
		t.Fatal("commit replaced an existing final object")
	}
}

func writeLocalUpload(t *testing.T, store *Store, key, value string) {
	t.Helper()
	target, err := store.CreateUploadTarget(context.Background(), key, 1024, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, target.URL, bytes.NewBufferString(value))
	request.SetPathValue("token", request.URL.Path[len("/dev/uploads/"):])
	response := httptest.NewRecorder()
	store.PutHandler(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", response.Code)
	}
}
