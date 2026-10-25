package assets

import (
	"context"
	"encoding/hex"
)

// Only the owning CMS can read clean, source-bound illustrations without a grant.
func (s *Service) BulletinIllustrationMetadata(ctx context.Context, caller, issueID, assetID, checksum string) (PublicDownloadMetadata, error) {
	if caller != "hhc-web-api" {
		return PublicDownloadMetadata{}, ErrForbidden
	}
	if _, err := hex.DecodeString(checksum); err != nil || len(checksum) != 64 || issueID == "" {
		return PublicDownloadMetadata{}, ErrInvalidInput
	}
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	if asset.ID != assetID || asset.OwnerService != caller || asset.OwnerType != "bulletin_issue" || asset.OwnerID != issueID || asset.Namespace != "cms.weekly.image" || asset.Purpose != "online_bulletin_illustration" || asset.Locale != "zh-Hant" || asset.Visibility != VisibilityPrivate || !asset.DeletedAt.IsZero() || asset.ChecksumSHA256 != checksum || asset.DetectedMIMEType != "image/png" || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean || asset.ProcessingStatus != ProcessingNotRequired || asset.SizeBytes < 1 || asset.SizeBytes > 2<<20 {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	return PublicDownloadMetadata{
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, FileName: asset.OriginalFileName, ETag: asset.ETag,
		LastModified: asset.UpdatedAt, CacheControl: "private, no-store", objectKey: asset.ObjectKey,
	}, nil
}
