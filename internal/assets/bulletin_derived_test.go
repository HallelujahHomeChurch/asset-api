package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCompleteDerivedBulletinPreservesByteValidationWithoutScanQueue(t *testing.T) {
	for _, invalid := range []string{"", "source pending", "source deleted", "source hash", "source owner", "source derived", "output owner", "output purpose", "output visibility", "output hash"} {
		t.Run(invalid, func(t *testing.T) {
			ctx := context.Background()
			repo, blobs := newMemoryRepository(), newMemoryBlobStore()
			service := NewService(repo, blobs, "https://example.invalid/assets", time.Now)
			source := Asset{ID: strings.Repeat("b", 32), Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_issue", Purpose: "weekly_bulletin", Locale: "zh-Hant", ExpectedMIMEType: "application/pdf", DetectedMIMEType: "application/pdf", UploadStatus: UploadCompleted, ScanStatus: ScanClean, Visibility: VisibilityPrivate, ChecksumSHA256: strings.Repeat("c", 64), ETag: "immutable"}
			created, err := service.CreateUploadSession(ctx, CreateUploadInput{Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_download_job", OwnerID: "job", Purpose: "weekly_bulletin_member", Locale: "zh-Hant", OriginalFileName: "weekly.pdf", ExpectedMIMEType: "application/pdf", MaxSizeBytes: 1024}, "derived")
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("%PDF-1.7\nserver rendered bulletin")
			blobs.objects[created.Session.StagingObjectKey] = payload
			sum := sha256.Sum256(payload)
			input := CompleteDerivedBulletinInput{CompleteUploadInput: CompleteUploadInput{SizeBytes: int64(len(payload)), MIMEType: "application/pdf", ChecksumSHA256: hex.EncodeToString(sum[:])}, SourceAssetID: source.ID, SourceSHA256: source.ChecksumSHA256}
			switch invalid {
			case "source pending":
				source.ScanStatus = ScanPending
			case "source deleted":
				source.DeletedAt = time.Now()
			case "source hash":
				input.SourceSHA256 = strings.Repeat("d", 64)
			case "source owner":
				source.OwnerService = "account-api"
			case "source derived":
				source.OwnerType = "bulletin_download_job"
			case "output owner":
				created.Asset.OwnerType = "bulletin_issue"
			case "output purpose":
				created.Asset.Purpose = "weekly_bulletin"
			case "output visibility":
				created.Asset.Visibility = VisibilityPublic
			case "output hash":
				input.ChecksumSHA256 = strings.Repeat("e", 64)
			}
			repo.assets[source.ID], repo.assets[created.Asset.ID] = source, created.Asset
			asset, err := service.CompleteDerivedBulletin(ctx, created.Asset.ID, input)
			if invalid != "" {
				if err == nil {
					t.Fatal("invalid derivation accepted")
				}
				if len(repo.scanRequests) != 0 {
					t.Fatal("invalid derivation queued a scan")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if asset.ScanStatus != ScanClean || asset.ScanSignature != "trusted-bulletin-watermark-v1" || len(repo.scanRequests) != 0 || asset.ETag == "" {
				t.Fatalf("asset=%+v scans=%d", asset, len(repo.scanRequests))
			}
			if _, err := service.CompleteDerivedBulletin(ctx, asset.ID, input); err != nil {
				t.Fatalf("replay: %v", err)
			}
			input.SourceSHA256 = strings.Repeat("f", 64)
			if _, err := service.CompleteDerivedBulletin(ctx, asset.ID, input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("replay changed source: %v", err)
			}
		})
	}
}
