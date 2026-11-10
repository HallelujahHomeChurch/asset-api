package assets

import (
	"context"
	"strings"
)

const (
	TrustedBulletinWatermark   = "trusted-bulletin-watermark-v1"
	BulletinWatermarkTransform = "bulletin-watermark"
	BulletinWatermarkVersion   = "1"
)

type Derivation struct {
	SourceAssetID    string `json:"sourceAssetId"`
	SourceSHA256     string `json:"sourceSha256"`
	SourceETag       string `json:"sourceETag"`
	Transform        string `json:"transform"`
	TransformVersion string `json:"transformVersion"`
}

func (d Derivation) Valid() bool {
	return d.SourceAssetID != "" && len(d.SourceAssetID) <= 255 && len(d.SourceSHA256) == 64 &&
		strings.Trim(d.SourceSHA256, "0123456789abcdef") == "" && strings.TrimSpace(d.SourceETag) != "" &&
		len(d.SourceETag) <= 255 && d.Transform == BulletinWatermarkTransform && d.TransformVersion == BulletinWatermarkVersion
}

func (d Derivation) MatchesBulletin(source, output Asset) bool {
	return d.Valid() && source.ID == d.SourceAssetID && source.ID != output.ID &&
		source.Namespace == "cms.weekly.pdf" && source.OwnerService == "hhc-web-api" &&
		source.OwnerType == "bulletin_issue" && source.Purpose == "weekly_bulletin" &&
		source.Visibility == VisibilityPrivate && source.UploadStatus == UploadCompleted && source.ScanStatus == ScanClean &&
		source.ScanSignature != TrustedBulletinWatermark && source.Derivation == (Derivation{}) &&
		(source.ProcessingStatus == ProcessingNotRequired || source.ProcessingStatus == ProcessingReady) &&
		source.DetectedMIMEType == "application/pdf" && source.ETag == d.SourceETag && source.ChecksumSHA256 == d.SourceSHA256 &&
		source.DeletedAt.IsZero() && source.PurgedAt.IsZero() &&
		output.Namespace == "cms.weekly.pdf" && output.OwnerService == "hhc-web-api" &&
		output.OwnerType == "bulletin_download_job" && output.Purpose == "weekly_bulletin_member" &&
		output.Visibility == VisibilityPrivate && output.ExpectedMIMEType == "application/pdf" &&
		output.Locale == source.Locale && output.DeletedAt.IsZero() && output.PurgedAt.IsZero()
}

type CompleteDerivedBulletinInput struct {
	CompleteUploadInput
	SourceAssetID string `json:"sourceAssetId"`
	SourceSHA256  string `json:"sourceSha256"`
}

// CompleteDerivedBulletin retains the narrow legacy watermark worker adapter.
func (s *Service) CompleteDerivedBulletin(ctx context.Context, id string, input CompleteDerivedBulletinInput) (Asset, error) {
	proof := Derivation{
		SourceAssetID: input.SourceAssetID, SourceSHA256: input.SourceSHA256,
		Transform: BulletinWatermarkTransform, TransformVersion: BulletinWatermarkVersion,
	}
	return s.completeUpload(ctx, id, input.CompleteUploadInput, proof, true)
}

func (s *Service) CompleteDerivedUpload(ctx context.Context, id string, input CompleteUploadInput, proof Derivation) (Asset, error) {
	if !s.derivationProofEnabled {
		return Asset{}, ErrForbidden
	}
	if !proof.Valid() || proof.SourceAssetID == id {
		return Asset{}, ErrInvalidInput
	}
	return s.completeUpload(ctx, id, input, proof, false)
}

func (d Derivation) legacyDetails() string {
	return d.SourceAssetID + ":" + d.SourceSHA256
}

func (d Derivation) matchesCompleted(asset Asset, legacy bool) bool {
	return asset.ScanStatus == ScanClean && asset.ScanSignature == TrustedBulletinWatermark &&
		(asset.Derivation == d || (legacy && asset.Derivation == (Derivation{}) && asset.ScanDetails == d.legacyDetails()))
}
