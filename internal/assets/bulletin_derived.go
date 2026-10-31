package assets

import (
	"context"
	"strings"
)

const TrustedBulletinWatermark = "trusted-bulletin-watermark-v1"

type CompleteDerivedBulletinInput struct {
	CompleteUploadInput
	SourceAssetID string `json:"sourceAssetId"`
	SourceSHA256  string `json:"sourceSha256"`
}

// Only the authenticated watermark worker may attest to this controlled derivation.
func (s *Service) CompleteDerivedBulletin(ctx context.Context, id string, input CompleteDerivedBulletinInput) (Asset, error) {
	output, err := s.repository.GetAsset(ctx, id)
	if err != nil {
		return Asset{}, err
	}
	source, err := s.repository.GetAsset(ctx, input.SourceAssetID)
	if err != nil {
		return Asset{}, err
	}
	if output.Namespace != "cms.weekly.pdf" || output.OwnerService != "hhc-web-api" || output.OwnerType != "bulletin_download_job" || output.Purpose != "weekly_bulletin_member" || output.Visibility != VisibilityPrivate || output.ExpectedMIMEType != "application/pdf" || !output.DeletedAt.IsZero() || !output.PurgedAt.IsZero() ||
		source.ID == output.ID || source.Namespace != "cms.weekly.pdf" || source.OwnerService != output.OwnerService || source.OwnerType != "bulletin_issue" || source.Purpose != "weekly_bulletin" || source.Locale != output.Locale || source.Visibility != VisibilityPrivate || source.UploadStatus != UploadCompleted || source.ScanStatus != ScanClean || source.DetectedMIMEType != "application/pdf" || source.ETag == "" || !source.DeletedAt.IsZero() || !source.PurgedAt.IsZero() || len(input.SourceSHA256) != 64 || strings.Trim(input.SourceSHA256, "0123456789abcdef") != "" || source.ChecksumSHA256 != input.SourceSHA256 {
		return Asset{}, ErrInvalidInput
	}
	proof := source.ID + ":" + source.ChecksumSHA256
	if output.UploadStatus == UploadCompleted && (output.ScanSignature != TrustedBulletinWatermark || output.ScanDetails != proof) {
		return Asset{}, ErrConflict
	}
	asset, err := s.completeUpload(ctx, id, input.CompleteUploadInput, proof)
	if err != nil {
		return Asset{}, err
	}
	// A competing ordinary completion must never be reported as trusted/clean.
	asset, err = s.repository.GetAsset(ctx, id)
	if err != nil {
		return Asset{}, err
	}
	if asset.ScanStatus != ScanClean || asset.ScanSignature != TrustedBulletinWatermark || asset.ScanDetails != proof {
		return Asset{}, ErrConflict
	}
	return asset, nil
}
