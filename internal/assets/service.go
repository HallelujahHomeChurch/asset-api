package assets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
)

const uploadTTL = 10 * time.Minute

var namespaceMIMEs = map[string]map[string]bool{
	"cms.weekly.pdf":              {"application/pdf": true},
	"cms.news.cover":              {"image/jpeg": true, "image/png": true, "image/webp": true},
	"cms.page.image":              {"image/jpeg": true, "image/png": true, "image/webp": true},
	"line.group.file":             {"application/pdf": true, "image/jpeg": true, "image/png": true, "image/webp": true},
	"desktop.cloud-folder.object": {"application/pdf": true, "image/jpeg": true, "image/png": true, "image/webp": true, "application/octet-stream": true},
}

type Service struct {
	repository    Repository
	blobs         BlobStore
	publicBaseURL string
	now           func() time.Time
}

func NewService(repository Repository, blobs BlobStore, publicBaseURL string, now func() time.Time) *Service {
	return &Service{repository: repository, blobs: blobs, publicBaseURL: strings.TrimRight(publicBaseURL, "/"), now: now}
}

func (s *Service) CreateUploadSession(ctx context.Context, input CreateUploadInput, idempotencyKey string) (CreatedUpload, error) {
	allowed, ok := namespaceMIMEs[input.Namespace]
	if !ok || !allowed[input.ExpectedMIMEType] || input.OwnerService == "" || input.OwnerType == "" || input.OwnerID == "" || input.MaxSizeBytes <= 0 || idempotencyKey == "" {
		return CreatedUpload{}, ErrInvalidInput
	}
	if input.Visibility == "" {
		input.Visibility = VisibilityPrivate
	}
	if replay, ok, err := s.replayUpload(ctx, input, idempotencyKey); err != nil || ok {
		return replay, err
	}
	now := s.now().UTC()
	assetID := newID()
	objectKey := path.Join(environmentPrefix(), input.Namespace, now.Format("2006"), now.Format("01"), assetID, "original")
	processing := ProcessingNotRequired
	if strings.HasPrefix(input.ExpectedMIMEType, "image/") {
		processing = ProcessingPending
	}
	asset := Asset{ID: assetID, Namespace: input.Namespace, OwnerService: input.OwnerService, OwnerType: input.OwnerType, OwnerID: input.OwnerID, Purpose: input.Purpose, Locale: input.Locale, OriginalFileName: sanitizeFileName(input.OriginalFileName), ObjectKey: objectKey, ExpectedMIMEType: input.ExpectedMIMEType, UploadStatus: UploadCreated, ScanStatus: ScanPending, ProcessingStatus: processing, Visibility: input.Visibility, CreatedAt: now, UpdatedAt: now}
	session := UploadSession{ID: newID(), AssetID: assetID, IdempotencyKey: idempotencyKey, MaxSizeBytes: input.MaxSizeBytes, Status: UploadCreated, ExpiresAt: now.Add(uploadTTL), CreatedAt: now}
	target, err := s.blobs.CreateUploadTarget(ctx, objectKey, input.MaxSizeBytes, session.ExpiresAt)
	if err != nil {
		return CreatedUpload{}, fmt.Errorf("create upload target: %w", err)
	}
	if err := s.repository.CreateUpload(ctx, asset, session); err != nil {
		if replay, ok, replayErr := s.replayUpload(ctx, input, idempotencyKey); replayErr == nil && ok {
			return replay, nil
		}
		return CreatedUpload{}, fmt.Errorf("create upload: %w", err)
	}
	return CreatedUpload{Asset: asset, Session: session, Target: target}, nil
}

func (s *Service) replayUpload(ctx context.Context, input CreateUploadInput, key string) (CreatedUpload, bool, error) {
	repository, ok := s.repository.(interface {
		FindUploadByIdempotency(context.Context, string) (Asset, UploadSession, error)
	})
	if !ok {
		return CreatedUpload{}, false, nil
	}
	asset, session, err := repository.FindUploadByIdempotency(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return CreatedUpload{}, false, nil
	}
	if err != nil {
		return CreatedUpload{}, false, err
	}
	if asset.Namespace != input.Namespace || asset.OwnerService != input.OwnerService || asset.OwnerType != input.OwnerType || asset.OwnerID != input.OwnerID || asset.ExpectedMIMEType != input.ExpectedMIMEType || session.MaxSizeBytes != input.MaxSizeBytes {
		return CreatedUpload{}, true, ErrInvalidInput
	}
	created := CreatedUpload{Asset: asset, Session: session}
	if session.Status == UploadCreated && s.now().Before(session.ExpiresAt) {
		created.Target, err = s.blobs.CreateUploadTarget(ctx, asset.ObjectKey, session.MaxSizeBytes, session.ExpiresAt)
		if err != nil {
			return CreatedUpload{}, true, fmt.Errorf("replay upload target: %w", err)
		}
	}
	return created, true, nil
}

func (s *Service) CompleteUpload(ctx context.Context, assetID string, input CompleteUploadInput) (Asset, error) {
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return Asset{}, err
	}
	session, err := s.repository.GetUploadSession(ctx, assetID)
	if err != nil {
		return Asset{}, err
	}
	if session.Status == UploadCompleted {
		if asset.SizeBytes == input.SizeBytes && asset.ChecksumSHA256 == strings.ToLower(input.ChecksumSHA256) && asset.DetectedMIMEType == input.MIMEType {
			return asset, nil
		}
		return Asset{}, ErrInvalidUpload
	}
	if s.now().After(session.ExpiresAt) {
		return Asset{}, ErrInvalidUpload
	}
	observed, err := s.blobs.Inspect(ctx, asset.ObjectKey)
	if err != nil {
		return Asset{}, fmt.Errorf("inspect blob: %w", err)
	}
	allowed := namespaceMIMEs[asset.Namespace][observed.DetectedMIMEType]
	if observed.Size <= 0 || observed.Size > session.MaxSizeBytes || observed.Size != input.SizeBytes || !allowed || observed.DetectedMIMEType != input.MIMEType || !strings.EqualFold(observed.ChecksumSHA256, input.ChecksumSHA256) {
		return Asset{}, ErrInvalidUpload
	}
	now := s.now().UTC()
	asset.SizeBytes = observed.Size
	asset.ChecksumSHA256 = strings.ToLower(observed.ChecksumSHA256)
	asset.DetectedMIMEType = observed.DetectedMIMEType
	asset.ETag = observed.ETag
	asset.UploadStatus = UploadCompleted
	asset.ScanStatus = ScanPending
	asset.UpdatedAt = now
	session.Status = UploadCompleted
	session.CompletedAt = now
	if err := s.repository.CompleteUpload(ctx, asset, session); err != nil {
		return Asset{}, fmt.Errorf("complete upload: %w", err)
	}
	return asset, nil
}

func (s *Service) CreateGrant(ctx context.Context, assetID string, input CreateGrantInput) (Grant, error) {
	if input.SubjectType == "" || input.SubjectID == "" || input.Permission == "" || input.IdempotencyKey == "" {
		return Grant{}, ErrInvalidInput
	}
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return Grant{}, err
	}
	if !asset.DeletedAt.IsZero() {
		return Grant{}, ErrNotFound
	}
	if input.SubjectType == SubjectPublic && input.Permission == PermissionRead && (asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean || (asset.ProcessingStatus != ProcessingReady && asset.ProcessingStatus != ProcessingNotRequired)) {
		return Grant{}, ErrInvalidUpload
	}
	grant := Grant{ID: newID(), AssetID: assetID, SubjectType: input.SubjectType, SubjectID: input.SubjectID, Permission: input.Permission, IdempotencyKey: input.IdempotencyKey, ExpiresAt: input.ExpiresAt, CreatedAt: s.now().UTC()}
	return s.repository.CreateGrant(ctx, grant)
}

func (s *Service) GetAsset(ctx context.Context, assetID string) (Asset, error) {
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil || !asset.DeletedAt.IsZero() {
		return Asset{}, ErrNotFound
	}
	return asset, nil
}

func (s *Service) RevokeGrant(ctx context.Context, assetID, grantID string) error {
	return s.repository.RevokeGrant(ctx, assetID, grantID, s.now().UTC())
}

func (s *Service) ApplyScanResult(ctx context.Context, result ScanResult) error {
	if result.EventID == "" || result.AssetID == "" || (result.Status != ScanClean && result.Status != ScanInfected && result.Status != ScanFailed) {
		return ErrInvalidInput
	}
	_, err := s.repository.ApplyScanResult(ctx, result, s.now().UTC())
	return err
}

func (s *Service) OpenPublic(ctx context.Context, assetID string, byteRange ByteRange) (BlobDownload, error) {
	return s.openPublic(ctx, assetID, "", byteRange)
}

func (s *Service) OpenPublicVariant(ctx context.Context, assetID, variant string, byteRange ByteRange) (BlobDownload, error) {
	if variant != "small" && variant != "medium" && variant != "large" {
		return BlobDownload{}, ErrNotFound
	}
	return s.openPublic(ctx, assetID, variant, byteRange)
}

func (s *Service) openPublic(ctx context.Context, assetID, variant string, byteRange ByteRange) (BlobDownload, error) {
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return BlobDownload{}, ErrNotFound
	}
	if asset.Visibility != VisibilityPublic || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean || !asset.DeletedAt.IsZero() || (asset.ProcessingStatus != ProcessingReady && asset.ProcessingStatus != ProcessingNotRequired) {
		return BlobDownload{}, ErrNotFound
	}
	allowed, err := s.repository.HasActiveGrant(ctx, assetID, SubjectPublic, "*", PermissionRead, s.now().UTC())
	if err != nil || !allowed {
		return BlobDownload{}, ErrNotFound
	}
	objectKey := asset.ObjectKey
	contentType := asset.DetectedMIMEType
	totalSize := asset.SizeBytes
	if variant != "" {
		repository, ok := s.repository.(interface {
			GetDerivative(context.Context, string, string) (Derivative, error)
		})
		if !ok {
			return BlobDownload{}, ErrNotFound
		}
		derivative, err := repository.GetDerivative(ctx, assetID, variant)
		if err != nil {
			return BlobDownload{}, ErrNotFound
		}
		objectKey, contentType, totalSize = derivative.ObjectKey, derivative.MIMEType, derivative.SizeBytes
	}
	download, err := s.blobs.Open(ctx, objectKey, byteRange)
	if err != nil {
		return BlobDownload{}, err
	}
	download.ContentType = contentType
	download.TotalSize = totalSize
	return download, nil
}

func (s *Service) PublicURL(assetID string) string { return s.publicBaseURL + "/public/" + assetID }

func inspectBytes(value []byte) BlobProperties {
	sum := sha256.Sum256(value)
	mime := http.DetectContentType(value)
	if bytes.HasPrefix(value, []byte("%PDF-")) {
		mime = "application/pdf"
	}
	return BlobProperties{Size: int64(len(value)), DetectedMIMEType: mime, ChecksumSHA256: hex.EncodeToString(sum[:])}
}

func newID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}
func sanitizeFileName(value string) string {
	value = path.Base(strings.TrimSpace(value))
	if len(value) > 255 {
		value = value[:255]
	}
	return value
}
func environmentPrefix() string { return "assets" }
