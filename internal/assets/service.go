package assets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const uploadTTL = 10 * time.Minute
const contentTicketTTL = 5 * time.Minute

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
	if !ok || policy.OwnerService != input.OwnerService || !policy.AllowsMIME(input.ExpectedMIMEType) || !matchesFileExtension(input.OriginalFileName, input.ExpectedMIMEType) || input.OwnerType == "" || input.OwnerID == "" || !policy.AllowsSize(input.ExpectedMIMEType, input.MaxSizeBytes) || idempotencyKey == "" {
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
	if metadata.Size <= 0 || metadata.Size > session.MaxSizeBytes || !policy.AllowsSize(asset.ExpectedMIMEType, metadata.Size) {
		if err := s.rejectUpload(ctx, asset, session); err != nil {
			return Asset{}, err
		}
		return Asset{}, ErrInvalidUpload
	}
	observed, err := s.blobs.Inspect(ctx, sourceKey, metadata.ETag, session.MaxSizeBytes)
	if err != nil {
		return Asset{}, fmt.Errorf("inspect blob: %w", err)
	}
	observed.DetectedMIMEType, err = s.verifyDetectedMIME(ctx, sourceKey, observed, asset.OriginalFileName, asset.ExpectedMIMEType)
	if err != nil {
		if !errors.Is(err, ErrInvalidUpload) {
			return Asset{}, fmt.Errorf("validate media: %w", err)
		}
		if rejectErr := s.rejectUpload(ctx, asset, session); rejectErr != nil {
			return Asset{}, rejectErr
		}
		return Asset{}, ErrInvalidUpload
	}
	if observed.Size != metadata.Size || (metadata.ETag != "" && observed.ETag != metadata.ETag) || observed.Size != input.SizeBytes || observed.DetectedMIMEType == "" || observed.DetectedMIMEType != input.MIMEType || !strings.EqualFold(observed.ChecksumSHA256, input.ChecksumSHA256) {
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
				committed, metadataErr = s.blobs.Inspect(ctx, asset.ObjectKey, finalMetadata.ETag, session.MaxSizeBytes)
			}
			if committed.Size == observed.Size && strings.EqualFold(committed.ChecksumSHA256, observed.ChecksumSHA256) {
				committed.DetectedMIMEType = observed.DetectedMIMEType
			}
			if metadataErr == nil && committed.Size == observed.Size && committed.DetectedMIMEType == observed.DetectedMIMEType && strings.EqualFold(committed.ChecksumSHA256, observed.ChecksumSHA256) {
				err = nil
			}
		}
		if err != nil {
			return Asset{}, fmt.Errorf("commit blob: %w", err)
		}
	}
	if committed.Size == observed.Size && strings.EqualFold(committed.ChecksumSHA256, observed.ChecksumSHA256) {
		committed.DetectedMIMEType = observed.DetectedMIMEType
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

func (s *Service) CreateCollection(ctx context.Context, input CreateCollectionInput) (Collection, error) {
	if input.Namespace == "" || input.Name == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return Collection{}, ErrInvalidInput
	}
	return s.repository.CreateCollection(ctx, input, s.now().UTC())
}

func (s *Service) RenameCollection(ctx context.Context, input RenameCollectionInput) (Collection, error) {
	if input.CollectionID == "" || input.Name == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return Collection{}, ErrInvalidInput
	}
	return s.repository.RenameCollection(ctx, input, s.now().UTC())
}

func (s *Service) DeleteCollection(ctx context.Context, input DeleteCollectionInput) (Collection, error) {
	if input.CollectionID == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return Collection{}, ErrInvalidInput
	}
	return s.repository.DeleteCollection(ctx, input, s.now().UTC())
}

func (s *Service) AddCollectionACL(ctx context.Context, input AddCollectionACLInput) (CollectionACLMutation, error) {
	if input.CollectionID == "" || (input.SubjectType != SubjectUser && input.SubjectType != SubjectRole) || input.SubjectID == "" || input.Permission != PermissionRead || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return CollectionACLMutation{}, ErrInvalidInput
	}
	return s.repository.AddCollectionACL(ctx, input, s.now().UTC())
}

func (s *Service) RevokeCollectionACL(ctx context.Context, input RevokeCollectionACLInput) (CollectionACLMutation, error) {
	if input.CollectionID == "" || input.ACLID == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return CollectionACLMutation{}, ErrInvalidInput
	}
	return s.repository.RevokeCollectionACL(ctx, input, s.now().UTC())
}

func (s *Service) AddCollectionItem(ctx context.Context, input AddCollectionItemInput) (CollectionItemMutation, error) {
	if input.CollectionID == "" || input.AssetID == "" || input.RemoteItemID == "" || input.DisplayName == "" || input.SourceRevision == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return CollectionItemMutation{}, ErrInvalidInput
	}
	return s.repository.AddCollectionItem(ctx, input, s.now().UTC())
}

func (s *Service) DeleteCollectionItem(ctx context.Context, input DeleteCollectionItemInput) (CollectionItemMutation, error) {
	if input.CollectionID == "" || input.ItemID == "" || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return CollectionItemMutation{}, ErrInvalidInput
	}
	return s.repository.DeleteCollectionItem(ctx, input, s.now().UTC())
}

func (s *Service) SetCollectionItemsRetention(ctx context.Context, input SetCollectionItemsRetentionInput) error {
	itemIDs, ok := normalizeCollectionItemIDs(input.ItemIDs)
	if input.CollectionID == "" || !ok || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return ErrInvalidInput
	}
	input.ItemIDs = itemIDs
	return s.repository.SetCollectionItemsRetention(ctx, input, s.now().UTC())
}

func (s *Service) DeleteCollectionItems(ctx context.Context, input DeleteCollectionItemsInput) (DeleteCollectionItemsResult, error) {
	itemIDs, ok := normalizeCollectionItemIDs(input.ItemIDs)
	if input.CollectionID == "" || !ok || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return DeleteCollectionItemsResult{}, ErrInvalidInput
	}
	input.ItemIDs = itemIDs
	return s.repository.DeleteCollectionItems(ctx, input, s.now().UTC())
}

func normalizeCollectionItemIDs(values []string) ([]string, bool) {
	if len(values) < 1 || len(values) > 100 {
		return nil, false
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len(value) == 36 {
			if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
				return nil, false
			}
			value = value[:8] + value[9:13] + value[14:18] + value[19:23] + value[24:]
		}
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 16 {
			return nil, false
		}
		unique[hex.EncodeToString(decoded)] = struct{}{}
	}
	itemIDs := make([]string, 0, len(unique))
	for value := range unique {
		itemIDs = append(itemIDs, value)
	}
	slices.Sort(itemIDs)
	return itemIDs, true
}

func (s *Service) RenameCollectionItem(ctx context.Context, input RenameCollectionItemInput) (ManagedCollectionItem, error) {
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if input.CollectionID == "" || input.ItemID == "" || !validCollectionItemDisplayName(input.DisplayName) || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return ManagedCollectionItem{}, ErrInvalidInput
	}
	return s.repository.RenameCollectionItem(ctx, input, s.now().UTC())
}

func (s *Service) ListAuthorizedCollections(ctx context.Context, subject CollectionSubject, cursor string, limit int) (CollectionPage, error) {
	if !validCollectionSubject(subject) {
		return CollectionPage{}, ErrForbidden
	}
	return s.repository.ListAuthorizedCollections(ctx, subject, cursor, limit)
}

func (s *Service) GetAuthorizedCollection(ctx context.Context, id string, subject CollectionSubject) (Collection, error) {
	if id == "" || !validCollectionSubject(subject) {
		return Collection{}, ErrForbidden
	}
	return s.repository.GetAuthorizedCollection(ctx, id, subject)
}

func (s *Service) GetAuthorizedCollectionItem(ctx context.Context, collectionID, itemID string, subject CollectionSubject) (CollectionItem, error) {
	if collectionID == "" || itemID == "" || !validCollectionSubject(subject) {
		return CollectionItem{}, ErrForbidden
	}
	return s.repository.GetAuthorizedCollectionItem(ctx, collectionID, itemID, subject)
}

func (s *Service) AuthorizedCollectionContentMetadata(ctx context.Context, collectionID, itemID string, subject CollectionSubject) (PublicDownloadMetadata, error) {
	item, err := s.GetAuthorizedCollectionItem(ctx, collectionID, itemID, subject)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	asset, err := s.repository.GetAsset(ctx, item.AssetID)
	if err != nil {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	metadata, err := collectionContentMetadata(asset, item.ETag)
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	metadata.FileName = item.DisplayName
	return metadata, nil
}

func (s *Service) IssueCollectionContentTicket(ctx context.Context, collectionID, itemID string, subject CollectionSubject, tokenExpiresAt time.Time) (ContentTicketResponse, error) {
	now := s.now().UTC()
	if !tokenExpiresAt.After(now) {
		return ContentTicketResponse{}, ErrUnauthorized
	}
	item, err := s.GetAuthorizedCollectionItem(ctx, collectionID, itemID, subject)
	if err != nil {
		return ContentTicketResponse{}, err
	}
	expiresAt := tokenExpiresAt.UTC()
	if maximum := now.Add(contentTicketTTL); expiresAt.After(maximum) {
		expiresAt = maximum
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ContentTicketResponse{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256(raw)
	ticket := ContentTicket{
		TokenHash: hex.EncodeToString(hash[:]), CollectionID: collectionID, CollectionItemID: itemID,
		AssetETag: item.ETag, UserID: subject.UserID, Roles: append([]string(nil), subject.Roles...),
		ExpiresAt: expiresAt, CreatedAt: now,
	}
	if err := s.repository.CreateContentTicket(ctx, ticket, now); err != nil {
		return ContentTicketResponse{}, err
	}
	return ContentTicketResponse{ContentURL: "/api/assets/content?ticket=" + token, ExpiresAt: expiresAt, ETag: item.ETag}, nil
}

func (s *Service) IssueManagedContentTickets(ctx context.Context, collectionID string, itemIDs []string, ttl time.Duration) (ManagedContentTicketBatch, error) {
	itemIDs, ok := normalizeCollectionItemIDs(itemIDs)
	if collectionID == "" || !ok || ttl <= 0 {
		return ManagedContentTicketBatch{}, ErrInvalidInput
	}
	now := s.now().UTC()
	expiresAt := now.Add(ttl)
	if maximum := now.Add(contentTicketTTL); expiresAt.After(maximum) {
		expiresAt = maximum
	}
	batch := ManagedContentTicketBatch{Tickets: []ManagedContentTicket{}, UnavailableItemIDs: []string{}}
	for _, itemID := range itemIDs {
		item, err := s.repository.GetManagedCollectionItem(ctx, collectionID, itemID)
		if errors.Is(err, ErrNotFound) {
			batch.UnavailableItemIDs = append(batch.UnavailableItemIDs, itemID)
			continue
		}
		if err != nil {
			return ManagedContentTicketBatch{}, err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return ManagedContentTicketBatch{}, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		hash := sha256.Sum256(raw)
		if err := s.repository.CreateContentTicket(ctx, ContentTicket{
			TokenHash: hex.EncodeToString(hash[:]), CollectionID: collectionID, CollectionItemID: itemID,
			AssetETag: item.ETag, UserID: "manager", Roles: []string{}, AccessMode: "manager", ExpiresAt: expiresAt, CreatedAt: now,
		}, now); err != nil {
			if errors.Is(err, ErrNotFound) {
				batch.UnavailableItemIDs = append(batch.UnavailableItemIDs, itemID)
				continue
			}
			return ManagedContentTicketBatch{}, err
		}
		batch.Tickets = append(batch.Tickets, ManagedContentTicket{ItemID: itemID, ContentURL: "/api/assets/content?ticket=" + token, ExpiresAt: expiresAt, ETag: item.ETag})
	}
	return batch, nil
}

func (s *Service) ContentTicketMetadata(ctx context.Context, token string) (PublicDownloadMetadata, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	canonical := base64.RawURLEncoding.EncodeToString(raw)
	if err != nil || len(raw) != 32 || subtle.ConstantTimeCompare([]byte(canonical), []byte(token)) != 1 {
		return PublicDownloadMetadata{}, ErrUnauthorized
	}
	hash := sha256.Sum256(raw)
	asset, err := s.repository.RedeemContentTicket(ctx, hex.EncodeToString(hash[:]), s.now().UTC())
	if err != nil {
		return PublicDownloadMetadata{}, err
	}
	return collectionContentMetadata(asset, asset.ETag)
}

func collectionContentMetadata(asset Asset, expectedETag string) (PublicDownloadMetadata, error) {
	if asset.ID == "" || asset.ETag == "" || asset.ETag != expectedETag || asset.UploadStatus != UploadCompleted || asset.ScanStatus != ScanClean || !asset.DeletedAt.IsZero() || (asset.ProcessingStatus != ProcessingReady && asset.ProcessingStatus != ProcessingNotRequired) {
		return PublicDownloadMetadata{}, ErrNotFound
	}
	return PublicDownloadMetadata{
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, FileName: asset.OriginalFileName,
		ETag: asset.ETag, LastModified: asset.UpdatedAt, CacheControl: "private, no-store", objectKey: asset.ObjectKey,
	}, nil
}

func (s *Service) CollectionChanges(ctx context.Context, id, cursor string, subject CollectionSubject) (CollectionChangePage, error) {
	if id == "" || !validCollectionSubject(subject) {
		return CollectionChangePage{}, ErrForbidden
	}
	return s.repository.CollectionChanges(ctx, id, cursor, subject)
}

func (s *Service) ListManagedCollections(ctx context.Context, callerService, cursor string, limit int) (ManagedCollectionPage, error) {
	if callerService == "" {
		return ManagedCollectionPage{}, ErrInvalidInput
	}
	return s.repository.ListManagedCollections(ctx, callerService, cursor, limit)
}

func (s *Service) GetManagedCollection(ctx context.Context, id, callerService string) (ManagedCollection, error) {
	if id == "" || callerService == "" {
		return ManagedCollection{}, ErrInvalidInput
	}
	return s.repository.GetManagedCollection(ctx, id, callerService)
}

func (s *Service) ListManagedCollectionItems(ctx context.Context, collectionID, query, cursor string, limit int) (ManagedCollectionItemPage, error) {
	if collectionID == "" || !validManagedCollectionItemQuery(query) {
		return ManagedCollectionItemPage{}, ErrInvalidInput
	}
	return s.repository.ListManagedCollectionItems(ctx, collectionID, query, cursor, limit)
}

func validManagedCollectionItemQuery(query string) bool {
	if query == "" {
		return true
	}
	if len(query) > 255 || !utf8.ValidString(query) {
		return false
	}
	for _, value := range query {
		if unicode.IsControl(value) {
			return false
		}
	}
	return true
}

func validCollectionItemDisplayName(value string) bool {
	return value != "" && len(value) <= 255 && !strings.ContainsAny(value, "/\\\\") && !strings.ContainsFunc(value, unicode.IsControl)
}

func (s *Service) UpdateCollectionRetention(ctx context.Context, input UpdateCollectionRetentionInput) (Collection, error) {
	if input.CollectionID == "" || input.RetentionDays < 1 || input.RetentionDays > 365 || !validMutationIdentity(input.CallerService, input.IdempotencyKey) {
		return Collection{}, ErrInvalidInput
	}
	return s.repository.UpdateCollectionRetention(ctx, input, s.now().UTC())
}

func validMutationIdentity(callerService, idempotencyKey string) bool {
	return callerService != "" && idempotencyKey != ""
}

func validCollectionSubject(subject CollectionSubject) bool {
	if subject.UserID == "" {
		return false
	}
	for _, role := range subject.Roles {
		if role == CollectionReaderRole {
			return true
		}
	}
	return false
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
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, FileName: asset.OriginalFileName, ETag: asset.ETag,
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
		metadata.FileName = ""
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
		Size: asset.SizeBytes, ContentType: asset.DetectedMIMEType, FileName: asset.OriginalFileName, ETag: asset.ETag,
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

func (s *Service) PublicURL(assetID string) string { return s.publicBaseURL + "/" + assetID }

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

func NormalizeDetectedMIME(expected, detected string) string {
	if expected == detected {
		return expected
	}
	if (expected == "text/plain" || expected == "text/markdown") && strings.HasPrefix(detected, "text/plain") {
		return expected
	}
	if detected == "application/zip" {
		switch expected {
		case "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			"application/vnd.apple.keynote",
			"application/vnd.oasis.opendocument.presentation",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
			return expected
		}
	}
	if detected == "application/octet-stream" {
		switch expected {
		case "application/vnd.ms-powerpoint", "application/msword", "application/vnd.ms-excel":
			return expected
		}
	}
	return ""
}

func (s *Service) verifyDetectedMIME(ctx context.Context, objectKey string, observed BlobProperties, fileName, expected string) (string, error) {
	rangeRequest := ByteRange{Offset: 0, Count: min(observed.Size, 512)}
	if requiresContentReader(expected) {
		rangeRequest = ByteRange{}
	}
	download, err := s.blobs.Open(ctx, objectKey, rangeRequest, observed.ETag)
	if err != nil {
		return "", err
	}
	defer download.Body.Close()
	if !requiresContentReader(expected) {
		header, err := io.ReadAll(io.LimitReader(download.Body, 512))
		if err != nil {
			return "", err
		}
		return ValidateMedia(ctx, fileName, expected, header, nil, observed.Size)
	}
	file, err := os.CreateTemp("", "asset-media-validation-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	defer func() {
		file.Close()
		os.Remove(name)
	}()
	written, err := io.Copy(file, io.LimitReader(&contextReader{ctx: ctx, reader: download.Body}, observed.Size+1))
	if err != nil {
		return "", err
	}
	if written != observed.Size {
		return "", ErrInvalidUpload
	}
	header := make([]byte, min(observed.Size, 512))
	read, err := file.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	return ValidateMedia(ctx, fileName, expected, header[:read], file, observed.Size)
}

func matchesFileExtension(fileName, mime string) bool {
	return extensionAllowed(fileName, mime)
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
