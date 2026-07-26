package assets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
)

const uploadTTL = 10 * time.Minute

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
	policy, ok := PolicyFor(input.Namespace)
	if !ok || policy.OwnerService != input.OwnerService || !policy.AllowsMIME(input.ExpectedMIMEType) || input.OwnerType == "" || input.OwnerID == "" || input.MaxSizeBytes <= 0 || input.MaxSizeBytes > policy.MaxSizeBytes || idempotencyKey == "" {
		return CreatedUpload{}, ErrInvalidInput
	}
	if input.Visibility == "" {
		input.Visibility = policy.DefaultVisibility
	}
	if !policy.AllowsVisibility(input.Visibility) {
		return CreatedUpload{}, ErrInvalidInput
	}
	if replay, ok, err := s.replayUpload(ctx, input, idempotencyKey); err != nil || ok {
		return replay, err
	}
	now := s.now().UTC()
	assetID := newID()
	sessionID := newID()
	objectKey := path.Join(environmentPrefix(), input.Namespace, now.Format("2006"), now.Format("01"), assetID, "original")
	stagingObjectKey := path.Join(environmentPrefix(), input.Namespace, now.Format("2006"), now.Format("01"), assetID, "staging", sessionID)
	asset := Asset{ID: assetID, Namespace: input.Namespace, OwnerService: input.OwnerService, OwnerType: input.OwnerType, OwnerID: input.OwnerID, Purpose: input.Purpose, Locale: input.Locale, OriginalFileName: sanitizeFileName(input.OriginalFileName), ObjectKey: objectKey, ExpectedMIMEType: input.ExpectedMIMEType, UploadStatus: UploadCreated, ScanStatus: ScanPending, ProcessingStatus: policy.Processing, Visibility: input.Visibility, CreatedAt: now, UpdatedAt: now}
	session := UploadSession{ID: sessionID, AssetID: assetID, IdempotencyKey: idempotencyKey, CallerService: input.OwnerService, Operation: "create_upload", Fingerprint: requestFingerprint(input), StagingObjectKey: stagingObjectKey, MaxSizeBytes: input.MaxSizeBytes, Status: UploadCreated, ExpiresAt: now.Add(uploadTTL), CreatedAt: now}
	target, err := s.blobs.CreateUploadTarget(ctx, stagingObjectKey, input.MaxSizeBytes, session.ExpiresAt)
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
		FindUploadByIdempotency(context.Context, string, string, string) (Asset, UploadSession, error)
	})
	if !ok {
		return CreatedUpload{}, false, nil
	}
	asset, session, err := repository.FindUploadByIdempotency(ctx, input.OwnerService, "create_upload", key)
	if errors.Is(err, ErrNotFound) {
		return CreatedUpload{}, false, nil
	}
	if err != nil {
		return CreatedUpload{}, false, err
	}
	if session.Fingerprint != requestFingerprint(input) {
		return CreatedUpload{}, true, ErrConflict
	}
	created := CreatedUpload{Asset: asset, Session: session}
	if session.Status == UploadCreated && s.now().Before(session.ExpiresAt) {
		created.Target, err = s.blobs.CreateUploadTarget(ctx, session.StagingObjectKey, session.MaxSizeBytes, session.ExpiresAt)
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
	observed, err := s.blobs.Inspect(ctx, session.StagingObjectKey)
	if err != nil {
		return Asset{}, fmt.Errorf("inspect blob: %w", err)
	}
	policy, ok := PolicyFor(asset.Namespace)
	if !ok || observed.Size <= 0 || observed.Size > session.MaxSizeBytes || observed.Size > policy.MaxSizeBytes || observed.Size != input.SizeBytes || !policy.AllowsMIME(observed.DetectedMIMEType) || observed.DetectedMIMEType != asset.ExpectedMIMEType || observed.DetectedMIMEType != input.MIMEType || !strings.EqualFold(observed.ChecksumSHA256, input.ChecksumSHA256) {
		return Asset{}, ErrInvalidUpload
	}
	committed, err := s.blobs.Commit(ctx, session.StagingObjectKey, asset.ObjectKey)
	if err != nil {
		return Asset{}, fmt.Errorf("commit blob: %w", err)
	}
	if committed.Size != observed.Size || committed.DetectedMIMEType != observed.DetectedMIMEType || !strings.EqualFold(committed.ChecksumSHA256, observed.ChecksumSHA256) {
		return Asset{}, ErrInvalidUpload
	}
	now := s.now().UTC()
	asset.SizeBytes = committed.Size
	asset.ChecksumSHA256 = strings.ToLower(committed.ChecksumSHA256)
	asset.DetectedMIMEType = committed.DetectedMIMEType
	asset.ETag = committed.ETag
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
	grant := Grant{ID: newID(), AssetID: assetID, SubjectType: input.SubjectType, SubjectID: input.SubjectID, Permission: input.Permission, IdempotencyKey: input.IdempotencyKey, CallerService: asset.OwnerService, Operation: "create_grant", Fingerprint: requestFingerprint(input), ExpiresAt: input.ExpiresAt, CreatedAt: s.now().UTC()}
	value, err := s.repository.CreateGrant(ctx, grant)
	if err == nil && value.Fingerprint != "" && value.Fingerprint != grant.Fingerprint {
		return Grant{}, ErrConflict
	}
	return value, err
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

func (s *Service) SoftDelete(ctx context.Context, assetID, ownerService string) error {
	repository, ok := s.repository.(interface {
		SoftDeleteAsset(context.Context, string, string, time.Time) error
	})
	if !ok {
		return ErrForbidden
	}
	return repository.SoftDeleteAsset(ctx, assetID, ownerService, s.now().UTC())
}

func (s *Service) RequeueScan(ctx context.Context, assetID, ownerService string) error {
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return err
	}
	if asset.OwnerService != ownerService {
		return ErrForbidden
	}
	if asset.ScanStatus != ScanFailed {
		return ErrInvalidInput
	}
	repository, ok := s.repository.(interface {
		RequeueFailedScan(context.Context, string, string, time.Time) error
	})
	if !ok {
		return ErrForbidden
	}
	return repository.RequeueFailedScan(ctx, assetID, ownerService, s.now().UTC())
}

func (s *Service) Operations(ctx context.Context) (Operations, error) {
	repository, ok := s.repository.(interface {
		GetOperations(context.Context, time.Time) (Operations, error)
	})
	if !ok {
		return Operations{}, ErrForbidden
	}
	return repository.GetOperations(ctx, s.now().UTC())
}

func (s *Service) ApplyScanResult(ctx context.Context, result ScanResult) error {
	if result.EventID == "" || result.AssetID == "" || (result.Status != ScanClean && result.Status != ScanInfected && result.Status != ScanFailed) {
		return ErrInvalidInput
	}
	if result.ETag == "" {
		asset, err := s.repository.GetAsset(ctx, result.AssetID)
		if err != nil {
			return err
		}
		result.ETag = asset.ETag
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
	expectedETag := asset.ETag
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
		expectedETag = derivative.ETag
	}
	download, err := s.blobs.Open(ctx, objectKey, byteRange, expectedETag)
	if err != nil {
		return BlobDownload{}, err
	}
	download.ContentType = contentType
	download.TotalSize = totalSize
	if policy, ok := PolicyFor(asset.Namespace); ok {
		download.CacheControl = policy.CacheControl
	}
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

func requestFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
