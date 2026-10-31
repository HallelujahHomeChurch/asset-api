package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/storage/local"
)

func TestDerivedBulletinWorkerHTTPCompletionUsesRealByteAndDatabasePath(t *testing.T) {
	db := integrationDB(t)
	store, ctx, now := New(db), context.Background(), time.Now().UTC()
	sourceID, sourceHash := strings.Repeat("b", 32), strings.Repeat("c", 64)
	insertAsset(t, db, sourceID, assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='cms.weekly.pdf',owner_service='hhc-web-api',owner_type='bulletin_issue',purpose='weekly_bulletin',locale='zh-Hant',visibility='private',detected_mime_type='application/pdf',checksum_sha256=$2,etag='source-etag' WHERE id=$1`, sourceID, sourceHash); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	blobs, err := local.New(root, "http://example.invalid/upload", "synthetic-signing-key")
	if err != nil {
		t.Fatal(err)
	}
	service := assets.NewService(store, blobs, "https://example.invalid/assets", time.Now)
	created, err := service.CreateUploadSession(ctx, assets.CreateUploadInput{Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_download_job", OwnerID: "synthetic-job", Purpose: "weekly_bulletin_member", Locale: "zh-Hant", OriginalFileName: "weekly.pdf", ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024}, "http-derived")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\nsynthetic server output")
	staging := filepath.Join(root, created.Session.StagingObjectKey)
	if err := os.MkdirAll(filepath.Dir(staging), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	body, _ := json.Marshal(assets.CompleteDerivedBulletinInput{CompleteUploadInput: assets.CompleteUploadInput{SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf"}, SourceAssetID: sourceID, SourceSHA256: sourceHash})
	r := httptest.NewRequest(http.MethodPost, "/priv/assets/bulletin-derivatives/"+created.Asset.ID+"/complete", bytes.NewReader(body))
	r.Header.Set("Dapr-Caller-App-Id", "hhc-web-bulletin-worker")
	r.Header.Set("dapr-api-token", "synthetic-app-token")
	w := httptest.NewRecorder()
	httpapi.New(service, db, map[string]bool{"hhc-web-api": true}, false, "synthetic-app-token", httpapi.WorkloadAuthConfig{}, nil).Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	asset, err := store.GetAsset(ctx, created.Asset.ID)
	if err != nil || asset.ScanStatus != assets.ScanClean || asset.ScanSignature != assets.TrustedBulletinWatermark || asset.ChecksumSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("asset=%+v err=%v", asset, err)
	}
	var queued int
	if err := db.QueryRow(`SELECT count(*) FROM asset_scan_outbox WHERE asset_id=$1`, asset.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
}
