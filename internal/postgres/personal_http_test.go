package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/storage/local"
)

func TestPersonalHTTPTransferAndRange(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	blobs, err := local.New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	handler := httpapi.New(assets.NewService(store, blobs, "", func() time.Time { return now }), db, nil, false, "token", httpapi.WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
	request := func(method, path, owner string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Dapr-Caller-App-Id", "api-gateway")
		r.Header.Set("dapr-api-token", "token")
		r.Header.Set("X-HHC-User-ID", owner)
		r.Header.Set("X-HHC-Scopes", "presenter:cloud:use")
		r.Header.Set("X-HHC-Token-ID", "session-token")
		r.Header.Set("X-HHC-Token-Expires-At", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
		r.Header.Set("X-HHC-Session-ID", "session")
		r.Header.Set("X-HHC-Auth-Provider", "account-api")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	mustStatus := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	mustStatus(request("POST", "/api/assets/personal-space", "alice", nil, nil), 200)
	payload := []byte("%PDF-1.7\nprivate")
	input, _ := json.Marshal(assets.PersonalUploadInput{FileName: "private.pdf", MIMEType: "application/pdf", SizeBytes: int64(len(payload))})
	created := request("POST", "/api/assets/personal-space/uploads", "alice", input, map[string]string{"Idempotency-Key": "op"})
	mustStatus(created, 201)
	var upload assets.PersonalUploadState
	if err = json.Unmarshal(created.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(created.Body.Bytes(), []byte("sig=")) || bytes.Contains(created.Body.Bytes(), []byte("objectKey")) {
		t.Fatal("private storage information exposed")
	}
	replay := request("POST", "/api/assets/personal-space/uploads", "alice", input, map[string]string{"Idempotency-Key": "op"})
	mustStatus(replay, 201)
	if replay.Body.String() != created.Body.String() {
		t.Fatal("upload receipt changed")
	}
	mustStatus(request("PUT", upload.ContentPath, "bob", payload, nil), 404)
	mustStatus(request("PUT", upload.ContentPath, "alice", payload, nil), 204)
	mustStatus(request("PUT", upload.ContentPath, "alice", payload, nil), 409)
	sum := sha256.Sum256(payload)
	completion, _ := json.Marshal(assets.CompleteUploadInput{SizeBytes: int64(len(payload)), MIMEType: "application/pdf", ChecksumSHA256: hex.EncodeToString(sum[:])})
	mustStatus(request("POST", "/api/assets/personal-space/uploads/"+upload.ID+"/complete", "alice", completion, nil), 200)
	mutation, _ := json.Marshal(assets.PersonalMutation{OperationID: "file", Type: "create-file", ItemID: "file", Name: "private.pdf", UploadID: upload.ID})
	mustStatus(request("POST", "/api/assets/personal-space/mutations", "alice", mutation, nil), 409)
	assetID, err := store.PersonalUploadAssetID(context.Background(), "alice", upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE assets SET scan_status='infected' WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}
	mustStatus(request("POST", "/api/assets/personal-space/mutations", "alice", mutation, nil), 422)
	if _, err = db.Exec(`UPDATE assets SET scan_status='clean' WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}
	committed := request("POST", "/api/assets/personal-space/mutations", "alice", mutation, nil)
	mustStatus(committed, 200)
	var result assets.PersonalMutationResult
	if err = json.Unmarshal(committed.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	contentPath := "/api/assets/personal-space/items/file/content?revision=" + strconv.FormatInt(result.NodeRevision, 10)
	download := request("GET", contentPath, "alice", nil, map[string]string{"Range": "bytes=0-3"})
	mustStatus(download, 206)
	if download.Body.String() != "%PDF" || download.Header().Get("Content-Range") != "bytes 0-3/16" {
		t.Fatalf("range=%s body=%s", download.Header().Get("Content-Range"), download.Body.String())
	}
	mustStatus(request("GET", contentPath, "bob", nil, nil), 404)
	mustStatus(request("GET", contentPath, "alice", nil, map[string]string{"Range": "bytes=999-"}), 416)
	mustStatus(request("GET", "/api/assets/personal-space/uploads/"+upload.ID, "bob", nil, nil), 404)
	mustStatus(request("PUT", upload.ContentPath, "alice", payload, nil), 409)
	huge := request("POST", "/api/assets/personal-space/uploads", "alice", []byte(`{"fileName":"huge.pdf","mimeType":"application/pdf","sizeBytes":209715201}`), map[string]string{"Idempotency-Key": "huge"})
	mustStatus(huge, 413)
}
