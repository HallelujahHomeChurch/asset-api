package assets

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
)

type OwnerContentInput struct {
	SHA256    string
	OwnerType string
	OwnerID   string
}

func (s *Service) OwnerContentMetadata(ctx context.Context, caller, assetID string, input OwnerContentInput) (PublicDownloadMetadata, error) {
	if caller != "hhc-web-api" {
		return PublicDownloadMetadata{}, ErrForbidden
	}
	if _, err := hex.DecodeString(input.SHA256); err != nil || len(input.SHA256) != 64 || strings.ToLower(input.SHA256) != input.SHA256 || input.OwnerType == "" || input.OwnerID == "" {
		return PublicDownloadMetadata{}, ErrInvalidInput
	}
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	if asset.ID != assetID || asset.OwnerService != caller || asset.OwnerType != "bulletin_issue" ||
		asset.OwnerType != input.OwnerType || asset.OwnerID != input.OwnerID || asset.ChecksumSHA256 != input.SHA256 ||
		(asset.Namespace != "cms.weekly.image" && asset.Namespace != "cms.weekly.pdf") {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	policy, ok := policyForAsset(asset)
	if !ok || !policy.AllowsMIME(asset.DetectedMIMEType) || !policy.AllowsSize(asset.DetectedMIMEType, asset.SizeBytes) {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	return readyOwnerContentMetadata(asset)
}

func readyOwnerContentMetadata(asset Asset) (PublicDownloadMetadata, error) {
	if asset.Visibility != VisibilityPrivate || !asset.DeletedAt.IsZero() || !asset.PurgedAt.IsZero() ||
		asset.ObjectKey == "" || asset.ETag == "" || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean ||
		(asset.ProcessingStatus != ProcessingReady && asset.ProcessingStatus != ProcessingNotRequired) {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	return PublicDownloadMetadata{
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, FileName: asset.OriginalFileName, ETag: asset.ETag,
		LastModified: asset.UpdatedAt, CacheControl: "private, no-store", objectKey: asset.ObjectKey,
	}, nil
}

func (s *Service) OpenOwnerContent(ctx context.Context, caller, assetID string, input OwnerContentInput, expected PublicDownloadMetadata, byteRange ByteRange) (BlobDownload, error) {
	download, err := s.blobs.Open(ctx, expected.objectKey, byteRange, expected.ETag)
	if errors.Is(err, ErrInvalidUpload) || errors.Is(err, ErrConflict) {
		return BlobDownload{}, ErrNotFound
	}
	if err != nil {
		return BlobDownload{}, err
	}
	if download.Body == nil {
		return BlobDownload{}, errors.New("owner content body unavailable")
	}
	if download.ETag != expected.ETag || download.TotalSize != expected.Size {
		_ = download.Body.Close()
		return BlobDownload{}, ErrNotFound
	}
	// Recheck authority after storage I/O, before any response bytes are written.
	current, err := s.OwnerContentMetadata(ctx, caller, assetID, input)
	if err != nil {
		_ = download.Body.Close()
		return BlobDownload{}, err
	}
	if current.objectKey != expected.objectKey || current.ETag != expected.ETag || current.Size != expected.Size || current.ContentType != expected.ContentType {
		_ = download.Body.Close()
		return BlobDownload{}, ErrNotFound
	}
	return bindDownloadMetadata(download, expected), nil
}
