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
		asset, err = s.repository.GetAsset(ctx, assetID)
		if err != nil {
			return Asset{}, err
		}
		if asset.SizeBytes == input.SizeBytes && asset.ChecksumSHA256 == strings.ToLower(input.ChecksumSHA256) && asset.DetectedMIMEType == input.MIMEType {
			if err := s.blobs.Delete(ctx, session.StagingObjectKey); err != nil {
				return Asset{}, fmt.Errorf("delete committed staging blob: %w", err)
			}
			return asset, nil
		}
		return Asset{}, ErrInvalidUpload
	}
	if session.Status == UploadFailed {
		if err := s.deleteUploadObjects(ctx, asset, session); err != nil {
			return Asset{}, fmt.Errorf("delete failed upload: %w", err)
		}
		return Asset{}, ErrInvalidUpload
	}
	policy, ok := PolicyFor(asset.Namespace)
	if !ok {
		return Asset{}, ErrInvalidUpload
	}
	sourceKey := asset.ObjectKey
	metadata, err := s.blobs.InspectProperties(ctx, sourceKey)
	alreadyCommitted := err == nil
	if errors.Is(err, ErrNotFound) {
		sourceKey = session.StagingObjectKey
		metadata, err = s.blobs.InspectProperties(ctx, sourceKey)
	}
	if errors.Is(err, ErrNotFound) {
		if err := s.rejectUpload(ctx, asset, session); err != nil {
			return Asset{}, err
		}
		return Asset{}, ErrInvalidUpload
	}
	if err != nil {
		return Asset{}, fmt.Errorf("inspect blob properties: %w", err)
	}
	if !alreadyCommitted && s.now().After(session.ExpiresAt) {
		if err := s.rejectUpload(ctx, asset, session); err != nil {
			return Asset{}, err
		}
		return Asset{}, ErrInvalidUpload
	}
	if metadata.Size <= 0 || metadata.Size > session.MaxSizeBytes || metadata.Size > policy.MaxSizeBytes {
		if err := s.rejectUpload(ctx, asset, session); err != nil {
			return Asset{}, err
		}
		return Asset{}, ErrInvalidUpload
	}
	observed, err := s.blobs.Inspect(ctx, sourceKey, metadata.ETag, min(session.MaxSizeBytes, policy.MaxSizeBytes))
	if err != nil {
		return Asset{}, fmt.Errorf("inspect blob: %w", err)
	}
	if observed.Size != metadata.Size || (metadata.ETag != "" && observed.ETag != metadata.ETag) || observed.Size != input.SizeBytes || !policy.AllowsMIME(observed.DetectedMIMEType) || observed.DetectedMIMEType != asset.ExpectedMIMEType || observed.DetectedMIMEType != input.MIMEType || !strings.EqualFold(observed.ChecksumSHA256, input.ChecksumSHA256) {
		if err := s.rejectUpload(ctx, asset, session); err != nil {
			return Asset{}, err
		}
		return Asset{}, ErrInvalidUpload
	}
	committed := observed
	if !alreadyCommitted {
		committed, err = s.blobs.Commit(ctx, session.StagingObjectKey, asset.ObjectKey)
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidUpload) {
			finalMetadata, metadataErr := s.blobs.InspectProperties(ctx, asset.ObjectKey)
			if metadataErr == nil && finalMetadata.Size == observed.Size {
				committed, metadataErr = s.blobs.Inspect(ctx, asset.ObjectKey, finalMetadata.ETag, min(session.MaxSizeBytes, policy.MaxSizeBytes))
			}
			if metadataErr == nil && committed.Size == observed.Size && committed.DetectedMIMEType == observed.DetectedMIMEType && strings.EqualFold(committed.ChecksumSHA256, observed.ChecksumSHA256) {
				err = nil
			}
		}
		if err != nil {
			return Asset{}, fmt.Errorf("commit blob: %w", err)
		}
	}
	if committed.Size != observed.Size || committed.DetectedMIMEType != observed.DetectedMIMEType || !strings.EqualFold(committed.ChecksumSHA256, observed.ChecksumSHA256) {
		if err := s.rejectUpload(ctx, asset, session); err != nil {
			return Asset{}, err
		}
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
	request := ScanRequest{EventID: newID(), AssetID: asset.ID, ETag: asset.ETag, CreatedAt: now}
	if err := s.repository.CompleteUpload(ctx, asset, session, request); err != nil {
		return Asset{}, fmt.Errorf("complete upload: %w", err)
	}
	if err := s.blobs.Delete(ctx, session.StagingObjectKey); err != nil {
		return Asset{}, fmt.Errorf("delete committed staging blob: %w", err)
	}
	return asset, nil
}

func (s *Service) rejectUpload(ctx context.Context, asset Asset, session UploadSession) error {
	if err := s.repository.FailUpload(ctx, asset.ID, s.now().UTC()); err != nil {
		return fmt.Errorf("mark upload failed: %w", err)
	}
	return s.deleteUploadObjects(ctx, asset, session)
}

func (s *Service) deleteUploadObjects(ctx context.Context, asset Asset, session UploadSession) error {
	return errors.Join(
		s.blobs.Delete(ctx, session.StagingObjectKey),
		s.blobs.Delete(ctx, asset.ObjectKey),
	)
}

func (s *Service) CreateGrant(ctx context.Context, assetID string, input CreateGrantInput) (Grant, error) {
	if !validSubjectType(input.SubjectType) || input.SubjectID == "" || (input.Permission != PermissionRead && input.Permission != PermissionDelete) || input.IdempotencyKey == "" {
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
		RequeueFailedScan(context.Context, string, string, ScanRequest, time.Time) error
	})
	if !ok {
		return ErrForbidden
	}
	now := s.now().UTC()
	request := ScanRequest{EventID: newID(), AssetID: asset.ID, ETag: asset.ETag, CreatedAt: now}
	return repository.RequeueFailedScan(ctx, assetID, ownerService, request, now)
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
	metadata, err := s.PublicMetadata(ctx, assetID, "")
	if err != nil {
		return BlobDownload{}, err
	}
	return s.OpenPublicMetadata(ctx, metadata, byteRange)
}

func (s *Service) OpenPublicVariant(ctx context.Context, assetID, variant string, byteRange ByteRange) (BlobDownload, error) {
	metadata, err := s.PublicMetadata(ctx, assetID, variant)
	if err != nil {
		return BlobDownload{}, err
	}
	return s.OpenPublicMetadata(ctx, metadata, byteRange)
}

func (s *Service) PublicMetadata(ctx context.Context, assetID, variant string) (PublicDownloadMetadata, error) {
	if variant != "" && variant != "small" && variant != "medium" && variant != "large" {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	if asset.Visibility != VisibilityPublic || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean || !asset.DeletedAt.IsZero() || (asset.ProcessingStatus != ProcessingReady && asset.ProcessingStatus != ProcessingNotRequired) {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	allowed, err := s.repository.HasActiveGrant(ctx, assetID, SubjectPublic, "*", PermissionRead, s.now().UTC())
	if err != nil || !allowed {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	metadata := PublicDownloadMetadata{
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, ETag: asset.ETag,
		LastModified: asset.UpdatedAt, objectKey: asset.ObjectKey,
	}
	if variant != "" {
		repository, ok := s.repository.(interface {
			GetDerivative(context.Context, string, string) (Derivative, error)
		})
		if !ok {
			return PublicDownloadMetadata{}, ErrNotFound
		}
		derivative, err := repository.GetDerivative(ctx, assetID, variant)
		if err != nil {
			return PublicDownloadMetadata{}, ErrNotFound
		}
		metadata.Size, metadata.ContentType, metadata.ETag = derivative.SizeBytes, derivative.MIMEType, derivative.ETag
		metadata.LastModified, metadata.objectKey = derivative.CreatedAt, derivative.ObjectKey
	}
	if policy, ok := PolicyFor(asset.Namespace); ok {
		metadata.CacheControl = policy.CacheControl
	}
	return metadata, nil
}

func (s *Service) AuthorizedMetadata(ctx context.Context, assetID string, subject SubjectType, subjectID string) (PublicDownloadMetadata, error) {
	if subject == SubjectPublic || !validSubjectType(subject) || subjectID == "" {
		return PublicDownloadMetadata{}, ErrInvalidInput
	}
	asset, err := s.repository.GetAsset(ctx, assetID)
	if err != nil || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean || !asset.DeletedAt.IsZero() || (asset.ProcessingStatus != ProcessingReady && asset.ProcessingStatus != ProcessingNotRequired) {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	allowed, err := s.repository.HasActiveGrant(ctx, assetID, subject, subjectID, PermissionRead, s.now().UTC())
	if err != nil || !allowed {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	return PublicDownloadMetadata{
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, ETag: asset.ETag,
		LastModified: asset.UpdatedAt, CacheControl: "private, no-store", objectKey: asset.ObjectKey,
	}, nil
}

func (s *Service) OpenPublicMetadata(ctx context.Context, metadata PublicDownloadMetadata, byteRange ByteRange) (BlobDownload, error) {
	download, err := s.blobs.Open(ctx, metadata.objectKey, byteRange, metadata.ETag)
	if err != nil {
		return BlobDownload{}, err
	}
	download.ContentType = metadata.ContentType
	download.TotalSize = metadata.Size
	download.ETag = metadata.ETag
	download.LastModified = metadata.LastModified
	download.CacheControl = metadata.CacheControl
	return download, nil
}

func (s *Service) PublicURL(assetID string) string { return s.publicBaseURL + "/public/" + assetID }

func validSubjectType(value SubjectType) bool {
	switch value {
	case SubjectPublic, SubjectUser, SubjectRole, SubjectService, SubjectLineGroup, SubjectAppClient:
		return true
	default:
		return false
	}
}

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
