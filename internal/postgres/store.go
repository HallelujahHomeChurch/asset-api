package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/lifecycle"

	"github.com/jackc/pgx/v5/pgconn"
)

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) CreateUpload(ctx context.Context, asset assets.Asset, session assets.UploadSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO assets (id,namespace,owner_service,owner_type,owner_id,purpose,locale,original_file_name,object_key,expected_mime_type,upload_status,scan_status,processing_status,visibility,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, asset.ID, asset.Namespace, asset.OwnerService, asset.OwnerType, asset.OwnerID, asset.Purpose, asset.Locale, asset.OriginalFileName, asset.ObjectKey, asset.ExpectedMIMEType, asset.UploadStatus, asset.ScanStatus, asset.ProcessingStatus, asset.Visibility, asset.CreatedAt, asset.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO upload_sessions (id,asset_id,idempotency_key,caller_service,operation,request_fingerprint,staging_object_key,max_size_bytes,status,expires_at,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, session.ID, session.AssetID, session.IdempotencyKey, session.CallerService, session.Operation, session.Fingerprint, session.StagingObjectKey, session.MaxSizeBytes, session.Status, session.ExpiresAt, session.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAsset(ctx context.Context, id string) (assets.Asset, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,namespace,owner_service,owner_type,owner_id,purpose,locale,original_file_name,object_key,expected_mime_type,detected_mime_type,size_bytes,checksum_sha256,etag,upload_status,scan_status,scan_details,scan_signature_version,scan_failure_category,processing_status,visibility,created_at,updated_at,COALESCE(deleted_at,'0001-01-01'::timestamptz),scan_attempts,scan_event_id,processing_attempts FROM assets WHERE id=$1 AND purged_at IS NULL`, id)
	var value assets.Asset
	err := row.Scan(&value.ID, &value.Namespace, &value.OwnerService, &value.OwnerType, &value.OwnerID, &value.Purpose, &value.Locale, &value.OriginalFileName, &value.ObjectKey, &value.ExpectedMIMEType, &value.DetectedMIMEType, &value.SizeBytes, &value.ChecksumSHA256, &value.ETag, &value.UploadStatus, &value.ScanStatus, &value.ScanDetails, &value.ScanSignature, &value.ScanFailure, &value.ProcessingStatus, &value.Visibility, &value.CreatedAt, &value.UpdatedAt, &value.DeletedAt, &value.ScanAttempts, &value.ScanEventID, &value.ProcessingAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.Asset{}, assets.ErrNotFound
	}
	return value, err
}

func (s *Store) GetUploadSession(ctx context.Context, assetID string) (assets.UploadSession, error) {
	var value assets.UploadSession
	err := s.db.QueryRowContext(ctx, `SELECT id,asset_id,idempotency_key,caller_service,operation,request_fingerprint,staging_object_key,max_size_bytes,status,expires_at,created_at,COALESCE(completed_at,'0001-01-01'::timestamptz) FROM upload_sessions WHERE asset_id=$1`, assetID).Scan(&value.ID, &value.AssetID, &value.IdempotencyKey, &value.CallerService, &value.Operation, &value.Fingerprint, &value.StagingObjectKey, &value.MaxSizeBytes, &value.Status, &value.ExpiresAt, &value.CreatedAt, &value.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.UploadSession{}, assets.ErrNotFound
	}
	return value, err
}

func (s *Store) FindUploadByIdempotency(ctx context.Context, caller, operation, key string) (assets.Asset, assets.UploadSession, error) {
	var assetID string
	if err := s.db.QueryRowContext(ctx, `SELECT asset_id FROM upload_sessions WHERE caller_service=$1 AND operation=$2 AND idempotency_key=$3`, caller, operation, key).Scan(&assetID); errors.Is(err, sql.ErrNoRows) {
		return assets.Asset{}, assets.UploadSession{}, assets.ErrNotFound
	} else if err != nil {
		return assets.Asset{}, assets.UploadSession{}, err
	}
	asset, err := s.GetAsset(ctx, assetID)
	if err != nil {
		return assets.Asset{}, assets.UploadSession{}, err
	}
	session, err := s.GetUploadSession(ctx, assetID)
	return asset, session, err
}

func (s *Store) CompleteUpload(ctx context.Context, asset assets.Asset, session assets.UploadSession, request assets.ScanRequest) error {
	if request.EventID == "" || request.AssetID != asset.ID || request.ETag == "" || request.ETag != asset.ETag || request.CreatedAt.IsZero() {
		return assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var assetStatus, sessionStatus string
	err = tx.QueryRowContext(ctx, `SELECT a.upload_status,u.status FROM assets a JOIN upload_sessions u ON u.asset_id=a.id WHERE a.id=$1 AND a.purged_at IS NULL FOR UPDATE OF a,u`, asset.ID).Scan(&assetStatus, &sessionStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	}
	if err != nil {
		return err
	}
	if assetStatus == string(assets.UploadCompleted) && sessionStatus == string(assets.UploadCompleted) {
		return tx.Commit()
	}
	if assetStatus != string(assets.UploadCreated) || sessionStatus != string(assets.UploadCreated) {
		return assets.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE assets SET detected_mime_type=$2,size_bytes=$3,checksum_sha256=$4,etag=$5,upload_status=$6,scan_status=$7,scan_event_id=$8,updated_at=$9 WHERE id=$1`, asset.ID, asset.DetectedMIMEType, asset.SizeBytes, asset.ChecksumSHA256, asset.ETag, asset.UploadStatus, asset.ScanStatus, request.EventID, asset.UpdatedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return assets.ErrNotFound
	}
	result, err = tx.ExecContext(ctx, `UPDATE upload_sessions SET status=$2,completed_at=$3 WHERE asset_id=$1`, asset.ID, session.Status, session.CompletedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return assets.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_scan_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES($1,$2,$3,$4,$4)`, request.EventID, request.AssetID, request.ETag, request.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimScanRequest(ctx context.Context, now time.Time, lease time.Duration) (assets.ScanRequest, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.ScanRequest{}, false, err
	}
	defer tx.Rollback()
	var request assets.ScanRequest
	err = tx.QueryRowContext(ctx, `
		SELECT event_id,asset_id,asset_etag,attempts,created_at
		FROM asset_scan_outbox
		WHERE delivered_at IS NULL AND available_at <= $1
		  AND (claimed_until IS NULL OR claimed_until < $1)
		ORDER BY available_at,created_at
		FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&request.EventID, &request.AssetID, &request.ETag, &request.Attempts, &request.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ScanRequest{}, false, nil
	}
	if err != nil {
		return assets.ScanRequest{}, false, err
	}
	request.Attempts++
	if _, err := tx.ExecContext(ctx, `UPDATE asset_scan_outbox SET attempts=$2,claimed_until=$3 WHERE event_id=$1`, request.EventID, request.Attempts, now.Add(lease)); err != nil {
		return assets.ScanRequest{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return assets.ScanRequest{}, false, err
	}
	return request, true, nil
}

func (s *Store) MarkScanRequestDelivered(ctx context.Context, eventID string, expectedAttempt int, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE asset_scan_outbox SET delivered_at=$3,claimed_until=NULL,last_error='' WHERE event_id=$1 AND attempts=$2 AND delivered_at IS NULL`, eventID, expectedAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return s.scanRequestTransitionError(ctx, eventID)
	}
	return nil
}

func (s *Store) ScheduleScanRequestRetry(ctx context.Context, eventID string, expectedAttempt int, details string, nextAttempt, _ time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE asset_scan_outbox SET available_at=$3,claimed_until=NULL,last_error=$4 WHERE event_id=$1 AND attempts=$2 AND delivered_at IS NULL`, eventID, expectedAttempt, nextAttempt, details)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return s.scanRequestTransitionError(ctx, eventID)
	}
	return nil
}

func (s *Store) scanRequestTransitionError(ctx context.Context, eventID string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_scan_outbox WHERE event_id=$1)`, eventID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return assets.ErrConflict
	}
	return assets.ErrNotFound
}

func (s *Store) FailUpload(ctx context.Context, assetID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var assetStatus, sessionStatus string
	err = tx.QueryRowContext(ctx, `SELECT a.upload_status,u.status FROM assets a JOIN upload_sessions u ON u.asset_id=a.id WHERE a.id=$1 AND a.purged_at IS NULL FOR UPDATE OF a,u`, assetID).Scan(&assetStatus, &sessionStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	}
	if err != nil {
		return err
	}
	if assetStatus == string(assets.UploadFailed) && sessionStatus == string(assets.UploadFailed) {
		return nil
	}
	if assetStatus != string(assets.UploadCreated) || sessionStatus != string(assets.UploadCreated) {
		return assets.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE assets SET upload_status='failed',updated_at=$2 WHERE id=$1`, assetID, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE upload_sessions SET status='failed' WHERE asset_id=$1`, assetID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return assets.ErrConflict
	}
	return tx.Commit()
}

func (s *Store) CreateGrant(ctx context.Context, grant assets.Grant) (assets.Grant, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO asset_grants (id,asset_id,subject_type,subject_id,permission,idempotency_key,caller_service,operation,request_fingerprint,expires_at,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'0001-01-01'::timestamptz),$11) ON CONFLICT (caller_service,operation,idempotency_key) DO NOTHING`, grant.ID, grant.AssetID, grant.SubjectType, grant.SubjectID, grant.Permission, grant.IdempotencyKey, grant.CallerService, grant.Operation, grant.Fingerprint, grant.ExpiresAt, grant.CreatedAt)
	if err != nil {
		return assets.Grant{}, err
	}
	var value assets.Grant
	err = s.db.QueryRowContext(ctx, `SELECT id,asset_id,subject_type,subject_id,permission,idempotency_key,caller_service,operation,request_fingerprint,COALESCE(expires_at,'0001-01-01'::timestamptz),created_at,COALESCE(revoked_at,'0001-01-01'::timestamptz) FROM asset_grants WHERE caller_service=$1 AND operation=$2 AND idempotency_key=$3`, grant.CallerService, grant.Operation, grant.IdempotencyKey).Scan(&value.ID, &value.AssetID, &value.SubjectType, &value.SubjectID, &value.Permission, &value.IdempotencyKey, &value.CallerService, &value.Operation, &value.Fingerprint, &value.ExpiresAt, &value.CreatedAt, &value.RevokedAt)
	return value, err
}

func (s *Store) RevokeGrant(ctx context.Context, assetID, grantID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE asset_grants SET revoked_at=$3 WHERE id=$1 AND asset_id=$2 AND revoked_at IS NULL`, grantID, assetID, now)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return assets.ErrNotFound
	}
	return nil
}
func (s *Store) HasActiveGrant(ctx context.Context, assetID string, subject assets.SubjectType, subjectID string, permission assets.Permission, now time.Time) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_grants WHERE asset_id=$1 AND subject_type=$2 AND subject_id=$3 AND permission=$4 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$5))`, assetID, subject, subjectID, permission, now).Scan(&exists)
	return exists, err
}

const (
	createCollectionOperation     = "create_collection"
	renameCollectionOperation     = "rename_collection"
	deleteCollectionOperation     = "delete_collection"
	addCollectionACLOperation     = "add_collection_acl"
	revokeCollectionACLOperation  = "revoke_collection_acl"
	addCollectionItemOperation    = "add_collection_item"
	deleteCollectionItemOperation = "delete_collection_item"
)

func (s *Store) CreateCollection(ctx context.Context, input assets.CreateCollectionInput, now time.Time) (assets.Collection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Collection{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ Namespace, Name string }{input.Namespace, input.Name})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, createCollectionOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.Collection{}, err
	}
	if !claimed {
		var value assets.Collection
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.Collection{}, err
		}
		value.CreatedByService = input.CallerService
		return value, nil
	}
	value := assets.Collection{ID: newStoreID(), Namespace: input.Namespace, Name: input.Name, Revision: 1, CreatedByService: input.CallerService, CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_collections(id,namespace,name,revision,created_by_service,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, value.ID, value.Namespace, value.Name, value.Revision, value.CreatedByService, now)
	if err != nil {
		return assets.Collection{}, mapCollectionError(err)
	}
	if err := finishMutation(ctx, tx, input.CallerService, createCollectionOperation, input.IdempotencyKey, value); err != nil {
		return assets.Collection{}, err
	}
	return value, nil
}

func (s *Store) RenameCollection(ctx context.Context, input assets.RenameCollectionInput, now time.Time) (assets.Collection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Collection{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ CollectionID, Name string }{input.CollectionID, input.Name})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, renameCollectionOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.Collection{}, err
	}
	if !claimed {
		var value assets.Collection
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.Collection{}, err
		}
		value.CreatedByService = input.CallerService
		return value, nil
	}
	value, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.Collection{}, err
	}
	value.Name, value.Revision, value.UpdatedAt = input.Name, value.Revision+1, now
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET name=$2,revision=$3,updated_at=$4 WHERE id=$1`, value.ID, value.Name, value.Revision, now); err != nil {
		return assets.Collection{}, err
	}
	if err := finishMutation(ctx, tx, input.CallerService, renameCollectionOperation, input.IdempotencyKey, value); err != nil {
		return assets.Collection{}, err
	}
	return value, nil
}

func (s *Store) DeleteCollection(ctx context.Context, input assets.DeleteCollectionInput, now time.Time) (assets.Collection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Collection{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ CollectionID string }{input.CollectionID})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, deleteCollectionOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.Collection{}, err
	}
	if !claimed {
		var value assets.Collection
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.Collection{}, err
		}
		value.CreatedByService = input.CallerService
		return value, nil
	}
	value, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.Collection{}, err
	}
	value.Revision, value.UpdatedAt, value.DeletedAt = value.Revision+1, now, now
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3,deleted_at=$3 WHERE id=$1`, value.ID, value.Revision, now); err != nil {
		return assets.Collection{}, err
	}
	if err := finishMutation(ctx, tx, input.CallerService, deleteCollectionOperation, input.IdempotencyKey, value); err != nil {
		return assets.Collection{}, err
	}
	return value, nil
}

func (s *Store) AddCollectionACL(ctx context.Context, input assets.AddCollectionACLInput, now time.Time) (assets.CollectionACLMutation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct {
		CollectionID string
		SubjectType  assets.SubjectType
		SubjectID    string
		Permission   assets.Permission
	}{input.CollectionID, input.SubjectType, input.SubjectID, input.Permission})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, addCollectionACLOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	if !claimed {
		var value assets.CollectionACLMutation
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.CollectionACLMutation{}, err
		}
		value.Collection.CreatedByService = input.CallerService
		return value, nil
	}
	collection, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	collection.Revision, collection.UpdatedAt = collection.Revision+1, now
	acl := assets.CollectionACL{ID: newStoreID(), CollectionID: collection.ID, SubjectType: input.SubjectType, SubjectID: input.SubjectID, Permission: input.Permission, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES($1,$2,$3,$4,$5,$6)`, acl.ID, acl.CollectionID, acl.SubjectType, acl.SubjectID, acl.Permission, now); err != nil {
		return assets.CollectionACLMutation{}, mapCollectionError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collection.ID, collection.Revision, now); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	value := assets.CollectionACLMutation{Collection: collection, ACL: acl}
	if err := finishMutation(ctx, tx, input.CallerService, addCollectionACLOperation, input.IdempotencyKey, value); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	return value, nil
}

func (s *Store) RevokeCollectionACL(ctx context.Context, input assets.RevokeCollectionACLInput, now time.Time) (assets.CollectionACLMutation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ CollectionID, ACLID string }{input.CollectionID, input.ACLID})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, revokeCollectionACLOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	if !claimed {
		var value assets.CollectionACLMutation
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.CollectionACLMutation{}, err
		}
		value.Collection.CreatedByService = input.CallerService
		return value, nil
	}
	collection, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	var acl assets.CollectionACL
	err = tx.QueryRowContext(ctx, `SELECT id,collection_id,subject_type,subject_id,permission,created_at FROM asset_collection_acl WHERE id=$1 AND collection_id=$2 AND revoked_at IS NULL FOR UPDATE`, input.ACLID, input.CollectionID).Scan(&acl.ID, &acl.CollectionID, &acl.SubjectType, &acl.SubjectID, &acl.Permission, &acl.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionACLMutation{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionACLMutation{}, err
	}
	collection.Revision, collection.UpdatedAt, acl.RevokedAt = collection.Revision+1, now, now
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collection_acl SET revoked_at=$2 WHERE id=$1`, acl.ID, now); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collection.ID, collection.Revision, now); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	value := assets.CollectionACLMutation{Collection: collection, ACL: acl}
	if err := finishMutation(ctx, tx, input.CallerService, revokeCollectionACLOperation, input.IdempotencyKey, value); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	return value, nil
}

func (s *Store) AddCollectionItem(ctx context.Context, input assets.AddCollectionItemInput, now time.Time) (assets.CollectionItemMutation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ CollectionID, AssetID, RemoteItemID, DisplayName, SourceRevision string }{input.CollectionID, input.AssetID, input.RemoteItemID, input.DisplayName, input.SourceRevision})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, addCollectionItemOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if !claimed {
		var value assets.CollectionItemMutation
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.CollectionItemMutation{}, err
		}
		value.Collection.CreatedByService = input.CallerService
		return value, nil
	}
	var mimeType, etag string
	var sizeBytes int64
	err = tx.QueryRowContext(ctx, `SELECT detected_mime_type,size_bytes,etag FROM assets WHERE id=$1 AND deleted_at IS NULL AND purged_at IS NULL FOR UPDATE`, input.AssetID).Scan(&mimeType, &sizeBytes, &etag)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionItemMutation{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	collection, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	collection.Revision, collection.UpdatedAt = collection.Revision+1, now
	item := assets.CollectionItem{ID: newStoreID(), CollectionID: collection.ID, AssetID: input.AssetID, RemoteItemID: input.RemoteItemID, DisplayName: input.DisplayName, SourceRevision: input.SourceRevision, CreatedRevision: collection.Revision, MIMEType: mimeType, SizeBytes: sizeBytes, ETag: etag, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, item.ID, item.CollectionID, item.AssetID, item.RemoteItemID, item.DisplayName, item.SourceRevision, item.CreatedRevision, now)
	if err != nil {
		return assets.CollectionItemMutation{}, mapCollectionError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collection.ID, collection.Revision, now); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	value := assets.CollectionItemMutation{Collection: collection, Item: item}
	if err := finishMutation(ctx, tx, input.CallerService, addCollectionItemOperation, input.IdempotencyKey, value); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	return value, nil
}

func (s *Store) DeleteCollectionItem(ctx context.Context, input assets.DeleteCollectionItemInput, now time.Time) (assets.CollectionItemMutation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ CollectionID, ItemID string }{input.CollectionID, input.ItemID})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, deleteCollectionItemOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if !claimed {
		var value assets.CollectionItemMutation
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.CollectionItemMutation{}, err
		}
		value.Collection.CreatedByService = input.CallerService
		return value, nil
	}
	collection, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	var item assets.CollectionItem
	err = tx.QueryRowContext(ctx, `SELECT id,collection_id,COALESCE(asset_id,''),remote_item_id,display_name,source_revision,created_revision,created_at FROM asset_collection_items WHERE id=$1 AND collection_id=$2 AND deleted_revision IS NULL FOR UPDATE`, input.ItemID, input.CollectionID).Scan(&item.ID, &item.CollectionID, &item.AssetID, &item.RemoteItemID, &item.DisplayName, &item.SourceRevision, &item.CreatedRevision, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionItemMutation{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	collection.Revision, collection.UpdatedAt = collection.Revision+1, now
	item.DeletedRevision, item.DeletedAt = collection.Revision, now
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET deleted_revision=$2,deleted_at=$3 WHERE id=$1`, item.ID, item.DeletedRevision, now); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collection.ID, collection.Revision, now); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	value := assets.CollectionItemMutation{Collection: collection, Item: item, Tombstone: assets.CollectionTombstone{ID: item.ID, RemoteItemID: item.RemoteItemID, DeletedRevision: item.DeletedRevision, DeletedAt: now}}
	if err := finishMutation(ctx, tx, input.CallerService, deleteCollectionItemOperation, input.IdempotencyKey, value); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	return value, nil
}

func claimMutation(ctx context.Context, tx *sql.Tx, caller, operation, key, fingerprint string, now time.Time) ([]byte, bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO asset_collection_mutations(caller_service,operation,idempotency_key,request_fingerprint,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, caller, operation, key, fingerprint, now)
	if err != nil {
		return nil, false, err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil, true, nil
	}
	var storedFingerprint string
	var response []byte
	if err := tx.QueryRowContext(ctx, `SELECT request_fingerprint,response_json FROM asset_collection_mutations WHERE caller_service=$1 AND operation=$2 AND idempotency_key=$3 FOR UPDATE`, caller, operation, key).Scan(&storedFingerprint, &response); err != nil {
		return nil, false, err
	}
	if storedFingerprint != fingerprint || response == nil {
		return nil, false, assets.ErrConflict
	}
	return response, false, nil
}

func finishMutation(ctx context.Context, tx *sql.Tx, caller, operation, key string, value any) error {
	response, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE asset_collection_mutations SET response_json=$4::jsonb WHERE caller_service=$1 AND operation=$2 AND idempotency_key=$3 AND response_json IS NULL`, caller, operation, key, response)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrConflict
	}
	return tx.Commit()
}

func lockManagedCollection(ctx context.Context, tx *sql.Tx, id, caller string) (assets.Collection, error) {
	var value assets.Collection
	err := tx.QueryRowContext(ctx, `SELECT id,namespace,name,revision,created_by_service,created_at,updated_at,COALESCE(deleted_at,'0001-01-01'::timestamptz) FROM asset_collections WHERE id=$1 FOR UPDATE`, id).Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt, &value.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) || (!value.DeletedAt.IsZero() && err == nil) {
		return assets.Collection{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.Collection{}, err
	}
	if value.CreatedByService != caller {
		return assets.Collection{}, assets.ErrForbidden
	}
	return value, nil
}

func mutationFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func newStoreID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

func mapCollectionError(err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return assets.ErrConflict
	}
	return err
}

const (
	collectionListLimit  = 100
	collectionEventLimit = 500
	changeModeReset      = "reset"
	changeModeDelta      = "delta"
)

type listCursor struct {
	LastID string `json:"i"`
}

type changeCursor struct {
	Mode          string `json:"m"`
	CollectionID  string `json:"c"`
	HighWater     int64  `json:"h,omitempty"`
	LastItemID    string `json:"i,omitempty"`
	FromRevision  int64  `json:"f,omitempty"`
	ToRevision    int64  `json:"t,omitempty"`
	AfterRevision int64  `json:"r,omitempty"`
	AfterKind     int    `json:"k,omitempty"`
	AfterID       string `json:"a,omitempty"`
}

func (s *Store) ListAuthorizedCollections(ctx context.Context, subject assets.CollectionSubject, cursor string, limit int) (assets.CollectionPage, error) {
	if !validReaderSubject(subject) {
		return assets.CollectionPage{}, assets.ErrForbidden
	}
	lastID := ""
	if decoded, ok := decodeListCursor(cursor); ok {
		lastID = decoded.LastID
	}
	limit = boundedCollectionLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id,c.namespace,c.name,c.revision,c.created_by_service,c.created_at,c.updated_at
		FROM asset_collections c
		WHERE c.deleted_at IS NULL AND c.id>$3
		  AND EXISTS (
		    SELECT 1 FROM asset_collection_acl acl
		    WHERE acl.collection_id=c.id AND acl.permission='read' AND acl.revoked_at IS NULL
		      AND ((acl.subject_type='user' AND acl.subject_id=$1)
		        OR (acl.subject_type='role' AND acl.subject_id=ANY($2::text[])))
		  )
		ORDER BY c.id LIMIT $4`, subject.UserID, subject.Roles, lastID, limit+1)
	if err != nil {
		return assets.CollectionPage{}, err
	}
	defer rows.Close()
	page := assets.CollectionPage{Collections: []assets.Collection{}}
	for rows.Next() {
		var value assets.Collection
		if err := rows.Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt); err != nil {
			return assets.CollectionPage{}, err
		}
		page.Collections = append(page.Collections, value)
	}
	if err := rows.Err(); err != nil {
		return assets.CollectionPage{}, err
	}
	if len(page.Collections) > limit {
		page.Collections = page.Collections[:limit]
		page.HasMore = true
		page.Cursor = encodeListCursor(listCursor{LastID: page.Collections[len(page.Collections)-1].ID})
	}
	return page, nil
}

func (s *Store) GetAuthorizedCollection(ctx context.Context, id string, subject assets.CollectionSubject) (assets.Collection, error) {
	value, err := s.getLiveCollection(ctx, id)
	if err != nil {
		return assets.Collection{}, err
	}
	if !validReaderSubject(subject) {
		return assets.Collection{}, assets.ErrForbidden
	}
	allowed, err := s.hasCollectionACL(ctx, id, subject)
	if err != nil {
		return assets.Collection{}, err
	}
	if !allowed {
		return assets.Collection{}, assets.ErrForbidden
	}
	return value, nil
}

func (s *Store) ListManagedCollections(ctx context.Context, callerService, cursor string, limit int) (assets.ManagedCollectionPage, error) {
	lastID := ""
	if decoded, ok := decodeListCursor(cursor); ok {
		lastID = decoded.LastID
	}
	limit = boundedCollectionLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id,namespace,name,revision,created_by_service,created_at,updated_at FROM asset_collections WHERE created_by_service=$1 AND deleted_at IS NULL AND id>$2 ORDER BY id LIMIT $3`, callerService, lastID, limit+1)
	if err != nil {
		return assets.ManagedCollectionPage{}, err
	}
	var collections []assets.Collection
	for rows.Next() {
		var value assets.Collection
		if err := rows.Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt); err != nil {
			rows.Close()
			return assets.ManagedCollectionPage{}, err
		}
		collections = append(collections, value)
	}
	if err := rows.Close(); err != nil {
		return assets.ManagedCollectionPage{}, err
	}
	page := assets.ManagedCollectionPage{Collections: []assets.ManagedCollection{}}
	if len(collections) > limit {
		collections = collections[:limit]
		page.HasMore = true
		page.Cursor = encodeListCursor(listCursor{LastID: collections[len(collections)-1].ID})
	}
	for _, collection := range collections {
		acls, err := s.collectionACLs(ctx, collection.ID)
		if err != nil {
			return assets.ManagedCollectionPage{}, err
		}
		page.Collections = append(page.Collections, assets.ManagedCollection{Collection: collection, ACLs: acls})
	}
	return page, nil
}

func (s *Store) GetManagedCollection(ctx context.Context, id, callerService string) (assets.ManagedCollection, error) {
	value, err := s.getLiveCollection(ctx, id)
	if err != nil {
		return assets.ManagedCollection{}, err
	}
	if value.CreatedByService != callerService {
		return assets.ManagedCollection{}, assets.ErrForbidden
	}
	acls, err := s.collectionACLs(ctx, id)
	if err != nil {
		return assets.ManagedCollection{}, err
	}
	return assets.ManagedCollection{Collection: value, ACLs: acls}, nil
}

func (s *Store) CollectionChanges(ctx context.Context, id, cursor string, subject assets.CollectionSubject) (assets.CollectionChangePage, error) {
	collection, err := s.GetAuthorizedCollection(ctx, id, subject)
	if err != nil {
		return assets.CollectionChangePage{}, err
	}
	decoded, valid := decodeChangeCursor(cursor)
	if !valid || !validChangeCursor(decoded, id, collection.Revision) {
		decoded = changeCursor{Mode: changeModeReset, CollectionID: id, HighWater: collection.Revision}
	}
	if decoded.Mode == changeModeReset {
		return s.collectionResetPage(ctx, collection, decoded, subject)
	}
	return s.collectionDeltaPage(ctx, collection, decoded, subject)
}

func (s *Store) collectionResetPage(ctx context.Context, collection assets.Collection, cursor changeCursor, subject assets.CollectionSubject) (assets.CollectionChangePage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id,i.collection_id,COALESCE(i.asset_id,''),i.remote_item_id,i.display_name,i.source_revision,
		       i.created_revision,i.created_at,COALESCE(a.detected_mime_type,''),COALESCE(a.size_bytes,0),COALESCE(a.etag,'')
		FROM asset_collection_items i
		LEFT JOIN assets a ON a.id=i.asset_id
		WHERE i.collection_id=$1 AND i.id>$2 AND i.created_revision<=$3
		  AND (i.deleted_revision IS NULL OR i.deleted_revision>$3)
		ORDER BY i.id LIMIT $4`, collection.ID, cursor.LastItemID, cursor.HighWater, collectionEventLimit+1)
	if err != nil {
		return assets.CollectionChangePage{}, err
	}
	defer rows.Close()
	page := assets.CollectionChangePage{Collection: collection, Items: []assets.CollectionItem{}, Tombstones: []assets.CollectionTombstone{}, Reset: true}
	for rows.Next() {
		var item assets.CollectionItem
		if err := rows.Scan(&item.ID, &item.CollectionID, &item.AssetID, &item.RemoteItemID, &item.DisplayName, &item.SourceRevision, &item.CreatedRevision, &item.CreatedAt, &item.MIMEType, &item.SizeBytes, &item.ETag); err != nil {
			return assets.CollectionChangePage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return assets.CollectionChangePage{}, err
	}
	if len(page.Items) > collectionEventLimit {
		page.Items = page.Items[:collectionEventLimit]
		page.HasMore = true
		page.Cursor = encodeChangeCursor(changeCursor{Mode: changeModeReset, CollectionID: collection.ID, HighWater: cursor.HighWater, LastItemID: page.Items[len(page.Items)-1].ID})
		return page, nil
	}
	current, err := s.GetAuthorizedCollection(ctx, collection.ID, subject)
	if err != nil {
		return assets.CollectionChangePage{}, err
	}
	page.Collection = current
	page.HasMore = true
	toRevision := cursor.HighWater
	if current.Revision > cursor.HighWater {
		toRevision = current.Revision
	}
	page.Cursor = encodeChangeCursor(changeCursor{Mode: changeModeDelta, CollectionID: collection.ID, FromRevision: cursor.HighWater, ToRevision: toRevision})
	return page, nil
}

func (s *Store) collectionDeltaPage(ctx context.Context, collection assets.Collection, cursor changeCursor, subject assets.CollectionSubject) (assets.CollectionChangePage, error) {
	if cursor.ToRevision <= cursor.FromRevision && cursor.AfterRevision == 0 {
		cursor.ToRevision = collection.Revision
	}
	afterRevision, afterKind := cursor.AfterRevision, cursor.AfterKind
	if afterRevision == 0 {
		afterRevision, afterKind = cursor.FromRevision, -1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind,id,remote_item_id,display_name,source_revision,event_revision,asset_id,mime_type,size_bytes,etag,event_at
		FROM (
		  SELECT 0 AS kind,i.id,i.remote_item_id,i.display_name,i.source_revision,i.created_revision AS event_revision,
		         COALESCE(i.asset_id,'') AS asset_id,COALESCE(a.detected_mime_type,'') AS mime_type,
		         COALESCE(a.size_bytes,0) AS size_bytes,COALESCE(a.etag,'') AS etag,i.created_at AS event_at
		  FROM asset_collection_items i LEFT JOIN assets a ON a.id=i.asset_id
		  WHERE i.collection_id=$1 AND i.created_revision>$2 AND i.created_revision<=$3
		  UNION ALL
		  SELECT 1 AS kind,i.id,i.remote_item_id,'' AS display_name,'' AS source_revision,i.deleted_revision AS event_revision,
		         '' AS asset_id,'' AS mime_type,0::bigint AS size_bytes,'' AS etag,i.deleted_at AS event_at
		  FROM asset_collection_items i
		  WHERE i.collection_id=$1 AND i.deleted_revision>$2 AND i.deleted_revision<=$3
		) events
		WHERE event_revision>$4 OR (event_revision=$4 AND (kind>$5 OR (kind=$5 AND id>$6)))
		ORDER BY event_revision,kind,id LIMIT $7`, collection.ID, cursor.FromRevision, cursor.ToRevision, afterRevision, afterKind, cursor.AfterID, collectionEventLimit+1)
	if err != nil {
		return assets.CollectionChangePage{}, err
	}
	type changeEvent struct {
		kind                                                                   int
		revision                                                               int64
		id, remoteItemID, displayName, sourceRevision, assetID, mimeType, etag string
		sizeBytes                                                              int64
		at                                                                     time.Time
	}
	var events []changeEvent
	for rows.Next() {
		var event changeEvent
		if err := rows.Scan(&event.kind, &event.id, &event.remoteItemID, &event.displayName, &event.sourceRevision, &event.revision, &event.assetID, &event.mimeType, &event.sizeBytes, &event.etag, &event.at); err != nil {
			rows.Close()
			return assets.CollectionChangePage{}, err
		}
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return assets.CollectionChangePage{}, err
	}
	page := assets.CollectionChangePage{Collection: collection, Items: []assets.CollectionItem{}, Tombstones: []assets.CollectionTombstone{}}
	moreEvents := len(events) > collectionEventLimit
	if moreEvents {
		events = events[:collectionEventLimit]
	}
	for _, event := range events {
		if event.kind == 0 {
			page.Items = append(page.Items, assets.CollectionItem{ID: event.id, CollectionID: collection.ID, AssetID: event.assetID, RemoteItemID: event.remoteItemID, DisplayName: event.displayName, SourceRevision: event.sourceRevision, CreatedRevision: event.revision, MIMEType: event.mimeType, SizeBytes: event.sizeBytes, ETag: event.etag, CreatedAt: event.at})
		} else {
			page.Tombstones = append(page.Tombstones, assets.CollectionTombstone{ID: event.id, RemoteItemID: event.remoteItemID, DeletedRevision: event.revision, DeletedAt: event.at})
		}
	}
	if moreEvents {
		last := events[len(events)-1]
		page.HasMore = true
		page.Cursor = encodeChangeCursor(changeCursor{Mode: changeModeDelta, CollectionID: collection.ID, FromRevision: cursor.FromRevision, ToRevision: cursor.ToRevision, AfterRevision: last.revision, AfterKind: last.kind, AfterID: last.id})
		return page, nil
	}
	current, err := s.GetAuthorizedCollection(ctx, collection.ID, subject)
	if err != nil {
		return assets.CollectionChangePage{}, err
	}
	page.Collection = current
	page.HasMore = current.Revision > cursor.ToRevision
	nextTo := cursor.ToRevision
	if page.HasMore {
		nextTo = current.Revision
	}
	page.Cursor = encodeChangeCursor(changeCursor{Mode: changeModeDelta, CollectionID: collection.ID, FromRevision: cursor.ToRevision, ToRevision: nextTo})
	return page, nil
}

func (s *Store) getLiveCollection(ctx context.Context, id string) (assets.Collection, error) {
	var value assets.Collection
	err := s.db.QueryRowContext(ctx, `SELECT id,namespace,name,revision,created_by_service,created_at,updated_at FROM asset_collections WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.Collection{}, assets.ErrNotFound
	}
	return value, err
}

func (s *Store) hasCollectionACL(ctx context.Context, id string, subject assets.CollectionSubject) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_collection_acl WHERE collection_id=$1 AND permission='read' AND revoked_at IS NULL AND ((subject_type='user' AND subject_id=$2) OR (subject_type='role' AND subject_id=ANY($3::text[]))))`, id, subject.UserID, subject.Roles).Scan(&allowed)
	return allowed, err
}

func (s *Store) collectionACLs(ctx context.Context, id string) ([]assets.CollectionACL, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,collection_id,subject_type,subject_id,permission,created_at,COALESCE(revoked_at,'0001-01-01'::timestamptz) FROM asset_collection_acl WHERE collection_id=$1 ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []assets.CollectionACL{}
	for rows.Next() {
		var value assets.CollectionACL
		if err := rows.Scan(&value.ID, &value.CollectionID, &value.SubjectType, &value.SubjectID, &value.Permission, &value.CreatedAt, &value.RevokedAt); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func validReaderSubject(subject assets.CollectionSubject) bool {
	if subject.UserID == "" {
		return false
	}
	for _, role := range subject.Roles {
		if role == assets.CollectionReaderRole {
			return true
		}
	}
	return false
}

func boundedCollectionLimit(limit int) int {
	if limit <= 0 || limit > collectionListLimit {
		return collectionListLimit
	}
	return limit
}

func encodeListCursor(cursor listCursor) string {
	value, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeListCursor(value string) (listCursor, bool) {
	if value == "" {
		return listCursor{}, true
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return listCursor{}, false
	}
	var cursor listCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return listCursor{}, false
	}
	return cursor, cursor.LastID != ""
}

func encodeChangeCursor(cursor changeCursor) string {
	value, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeChangeCursor(value string) (changeCursor, bool) {
	if value == "" {
		return changeCursor{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return changeCursor{}, false
	}
	var cursor changeCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.CollectionID == "" {
		return changeCursor{}, false
	}
	return cursor, true
}

func validChangeCursor(cursor changeCursor, collectionID string, revision int64) bool {
	if cursor.CollectionID != collectionID {
		return false
	}
	if cursor.Mode == changeModeReset {
		return cursor.HighWater > 0 && cursor.HighWater <= revision
	}
	if cursor.Mode != changeModeDelta || cursor.FromRevision < 1 || cursor.ToRevision < cursor.FromRevision || cursor.ToRevision > revision {
		return false
	}
	if cursor.AfterRevision == 0 {
		return cursor.AfterKind == 0 && cursor.AfterID == ""
	}
	return cursor.AfterRevision >= cursor.FromRevision && cursor.AfterRevision <= cursor.ToRevision && (cursor.AfterKind == 0 || cursor.AfterKind == 1) && cursor.AfterID != ""
}

func (s *Store) ApplyScanResult(ctx context.Context, result assets.ScanResult, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	insert, err := tx.ExecContext(ctx, `INSERT INTO asset_scan_events(event_id,asset_id,status,details,etag,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event_id) DO NOTHING`, result.EventID, result.AssetID, result.Status, result.Details, result.ETag, now)
	if err != nil {
		return false, err
	}
	affected, _ := insert.RowsAffected()
	if affected == 0 {
		return false, nil
	}
	updated, err := tx.ExecContext(ctx, `UPDATE assets SET scan_status=$2,scan_details=$3,scan_signature_version=$7,scan_failure_category=$8,scan_claimed_until=NULL,updated_at=$9 WHERE id=$1 AND scan_status='pending' AND etag=$4 AND scan_attempts=$5 AND scan_event_id=$6 AND deleted_at IS NULL AND purged_at IS NULL`, result.AssetID, result.Status, result.Details, result.ETag, result.ExpectedAttempt, result.EventID, result.Signature, result.FailureCategory, now)
	if err != nil {
		return false, err
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		return false, assets.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ClaimAssetScan distinguishes terminal events from work that only needs a
// later visibility retry, so queue delivery cannot be lost behind a DB lease.
func (s *Store) ClaimAssetScan(ctx context.Context, eventID, assetID, etag string, now time.Time, lease time.Duration) (assets.Asset, assets.ScanClaimState, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE assets
		SET scan_attempts=scan_attempts+1,scan_claimed_until=$4,updated_at=$3
		WHERE id=$1 AND etag=$2 AND scan_event_id=$5 AND upload_status='completed' AND scan_status='pending'
		  AND deleted_at IS NULL AND purged_at IS NULL
		  AND (scan_next_attempt_at IS NULL OR scan_next_attempt_at <= $3)
		  AND (scan_claimed_until IS NULL OR scan_claimed_until < $3)`, assetID, etag, now, now.Add(lease), eventID)
	if err != nil {
		return assets.Asset{}, "", err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		asset, err := s.GetAsset(ctx, assetID)
		return asset, assets.ScanClaimed, err
	}
	asset, err := s.GetAsset(ctx, assetID)
	if errors.Is(err, assets.ErrNotFound) {
		return assets.Asset{}, assets.ScanTerminal, nil
	}
	if err != nil {
		return assets.Asset{}, "", err
	}
	if asset.ScanStatus == assets.ScanPending && asset.ETag == etag && asset.ScanEventID == eventID {
		return asset, assets.ScanBusy, nil
	}
	return asset, assets.ScanTerminal, nil
}

func (s *Store) ScheduleAssetScanRetry(ctx context.Context, assetID string, expectedAttempt int, details, category string, nextAttempt, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET scan_details=$3,scan_failure_category=$4,scan_next_attempt_at=$5,scan_claimed_until=NULL,updated_at=$6 WHERE id=$1 AND scan_attempts=$2 AND scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL`, assetID, expectedAttempt, details, category, nextAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrConflict
	}
	return nil
}

func (s *Store) RecordScanPoison(ctx context.Context, poison assets.ScanPoison, now time.Time) (bool, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO asset_scan_poison_events(poison_id,event_id,asset_id,asset_etag,reason,details,dequeue_count,source_message_id,body_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(poison_id) DO NOTHING`, poison.PoisonID, poison.EventID, poison.AssetID, poison.ETag, poison.Reason, poison.Details, poison.DequeueCount, poison.SourceMessageID, poison.BodySHA256, now)
	if err != nil {
		return false, err
	}
	var shouldForward bool
	err = s.db.QueryRowContext(ctx, `SELECT forwarded_at IS NULL FROM asset_scan_poison_events WHERE poison_id=$1`, poison.PoisonID).Scan(&shouldForward)
	return shouldForward, err
}

func (s *Store) FailScanToPoison(ctx context.Context, result assets.ScanResult, poison assets.ScanPoison, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	insert, err := tx.ExecContext(ctx, `INSERT INTO asset_scan_events(event_id,asset_id,status,details,etag,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event_id) DO NOTHING`, result.EventID, result.AssetID, result.Status, result.Details, result.ETag, now)
	if err != nil {
		return false, err
	}
	if affected, _ := insert.RowsAffected(); affected == 1 {
		updated, err := tx.ExecContext(ctx, `UPDATE assets SET scan_status='failed',scan_details=$2,scan_signature_version=$6,scan_failure_category=$7,scan_claimed_until=NULL,updated_at=$8 WHERE id=$1 AND scan_status='pending' AND etag=$3 AND scan_attempts=$4 AND scan_event_id=$5 AND deleted_at IS NULL AND purged_at IS NULL`, result.AssetID, result.Details, result.ETag, result.ExpectedAttempt, result.EventID, result.Signature, result.FailureCategory, now)
		if err != nil {
			return false, err
		}
		if count, _ := updated.RowsAffected(); count != 1 {
			return false, assets.ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_scan_poison_events(poison_id,event_id,asset_id,asset_etag,reason,details,dequeue_count,source_message_id,body_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(poison_id) DO NOTHING`, poison.PoisonID, poison.EventID, poison.AssetID, poison.ETag, poison.Reason, poison.Details, poison.DequeueCount, poison.SourceMessageID, poison.BodySHA256, now); err != nil {
		return false, err
	}
	var shouldForward bool
	if err := tx.QueryRowContext(ctx, `SELECT forwarded_at IS NULL FROM asset_scan_poison_events WHERE poison_id=$1`, poison.PoisonID).Scan(&shouldForward); err != nil {
		return false, err
	}
	return shouldForward, tx.Commit()
}

func (s *Store) MarkScanPoisonForwarded(ctx context.Context, poisonID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asset_scan_poison_events SET forwarded_at=COALESCE(forwarded_at,$2) WHERE poison_id=$1`, poisonID, now)
	return err
}

func (s *Store) ClaimPendingScan(ctx context.Context, now time.Time, lease time.Duration) (assets.Asset, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Asset{}, false, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `
		SELECT id FROM assets
		WHERE upload_status='completed' AND scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL
		  AND (scan_next_attempt_at IS NULL OR scan_next_attempt_at <= $1)
		  AND (scan_claimed_until IS NULL OR scan_claimed_until < $1)
		ORDER BY COALESCE(scan_next_attempt_at, created_at), created_at
		FOR UPDATE SKIP LOCKED LIMIT 1`, now)
	var id string
	if err := row.Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return assets.Asset{}, false, nil
	} else if err != nil {
		return assets.Asset{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET scan_attempts=scan_attempts+1,scan_claimed_until=$2 WHERE id=$1 AND purged_at IS NULL`, id, now.Add(lease)); err != nil {
		return assets.Asset{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return assets.Asset{}, false, err
	}
	asset, err := s.GetAsset(ctx, id)
	return asset, err == nil, err
}

func (s *Store) ScheduleScanRetry(ctx context.Context, assetID string, expectedAttempt int, details string, nextAttempt, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET scan_details=$3,scan_next_attempt_at=$4,scan_claimed_until=NULL,updated_at=$5 WHERE id=$1 AND scan_attempts=$2 AND scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL`, assetID, expectedAttempt, details, nextAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1 AND deleted_at IS NULL AND purged_at IS NULL)`, assetID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return assets.ErrConflict
		}
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) SoftDeleteAsset(ctx context.Context, assetID, ownerService string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deletedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT deleted_at FROM assets WHERE id=$1 AND owner_service=$2 AND purged_at IS NULL FOR UPDATE`, assetID, ownerService).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	}
	if err != nil {
		return err
	}
	if deletedAt.Valid {
		return tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT collection_id FROM asset_collection_items WHERE asset_id=$1 AND deleted_revision IS NULL ORDER BY collection_id`, assetID)
	if err != nil {
		return err
	}
	var collectionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		collectionIDs = append(collectionIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(collectionIDs) > 0 {
		locked, err := tx.QueryContext(ctx, `SELECT id,revision FROM asset_collections WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE`, collectionIDs)
		if err != nil {
			return err
		}
		revisions := make(map[string]int64, len(collectionIDs))
		for locked.Next() {
			var id string
			var revision int64
			if err := locked.Scan(&id, &revision); err != nil {
				locked.Close()
				return err
			}
			revisions[id] = revision
		}
		if err := locked.Close(); err != nil {
			return err
		}
		for _, collectionID := range collectionIDs {
			nextRevision := revisions[collectionID] + 1
			result, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET deleted_revision=$3,deleted_at=$4 WHERE collection_id=$1 AND asset_id=$2 AND deleted_revision IS NULL`, collectionID, assetID, nextRevision, now)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collectionID, nextRevision, now); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET deleted_at=$2,updated_at=$2 WHERE id=$1`, assetID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RequeueFailedScan(ctx context.Context, assetID, ownerService string, request assets.ScanRequest, now time.Time) error {
	if request.EventID == "" || request.AssetID != assetID || request.ETag == "" || request.CreatedAt.IsZero() {
		return assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousEventID string
	err = tx.QueryRowContext(ctx, `SELECT scan_event_id FROM assets WHERE id=$1 AND owner_service=$2 AND etag=$3 AND scan_status='failed' AND deleted_at IS NULL AND purged_at IS NULL FOR UPDATE`, assetID, ownerService, request.ETag).Scan(&previousEventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return assets.ErrInvalidInput
		}
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assets SET scan_status='pending',scan_details='',scan_signature_version='',scan_failure_category='',scan_event_id=$2,scan_attempts=0,scan_next_attempt_at=$3,scan_claimed_until=NULL,updated_at=$3 WHERE id=$1`, assetID, request.EventID, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_scan_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES($1,$2,$3,$4,$4)`, request.EventID, request.AssetID, request.ETag, request.CreatedAt); err != nil {
		return err
	}
	if previousEventID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE asset_scan_poison_events SET replayed_at=COALESCE(replayed_at,$3),replay_event_id=CASE WHEN replay_event_id='' THEN $2 ELSE replay_event_id END WHERE event_id=$1 AND replayed_at IS NULL`, previousEventID, request.EventID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetOperations(ctx context.Context, now time.Time) (assets.Operations, error) {
	var value assets.Operations
	var oldestScan, oldestProcessing sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL),
		  COUNT(*) FILTER (WHERE scan_status='failed' AND deleted_at IS NULL AND purged_at IS NULL),
		  MIN(updated_at) FILTER (WHERE scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL),
		  COUNT(*) FILTER (WHERE upload_status='completed' AND scan_status='clean' AND processing_status='pending' AND deleted_at IS NULL AND purged_at IS NULL),
		  COUNT(*) FILTER (WHERE upload_status='completed' AND scan_status='clean' AND processing_status='failed' AND deleted_at IS NULL AND purged_at IS NULL),
		  MIN(updated_at) FILTER (WHERE upload_status='completed' AND scan_status='clean' AND processing_status='pending' AND deleted_at IS NULL AND purged_at IS NULL),
		  COUNT(*) FILTER (
			    WHERE purged_at IS NULL AND (
			      deleted_at IS NOT NULL OR
			      (upload_status='created' AND EXISTS (
			        SELECT 1 FROM upload_sessions u WHERE u.asset_id=assets.id AND u.expires_at < $1
			      )) OR
			      upload_status='failed' OR
			      (scan_status IN ('infected','failed') AND updated_at < $1 - interval '7 days') OR
			      (processing_status='failed' AND updated_at < $1 - interval '7 days')
		    )
		  )
		FROM assets`, now).Scan(
		&value.ScanPending, &value.ScanFailed, &oldestScan,
		&value.ProcessingPending, &value.ProcessingFailed, &oldestProcessing,
		&value.PurgePending,
	)
	if oldestScan.Valid {
		value.OldestScanPending = oldestScan.Time
	}
	if oldestProcessing.Valid {
		value.OldestProcessingPending = oldestProcessing.Time
	}
	return value, err
}

func (s *Store) ClaimPurge(ctx context.Context, now time.Time, lease time.Duration) (lifecycle.Candidate, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return lifecycle.Candidate{}, false, err
	}
	defer tx.Rollback()
	var assetID string
	err = tx.QueryRowContext(ctx, `
		SELECT a.id
		FROM assets a
		LEFT JOIN upload_sessions u ON u.asset_id=a.id
		WHERE a.purged_at IS NULL
		  AND (a.purge_next_attempt_at IS NULL OR a.purge_next_attempt_at <= $1)
		  AND (a.purge_claimed_until IS NULL OR a.purge_claimed_until < $1)
			  AND (
			    a.deleted_at IS NOT NULL OR
			    (a.upload_status='created' AND u.expires_at < $1) OR
			    a.upload_status='failed' OR
			    (a.scan_status IN ('infected','failed') AND a.updated_at < $1 - interval '7 days') OR
			    (a.processing_status='failed' AND a.updated_at < $1 - interval '7 days')
		  )
		ORDER BY a.updated_at
		FOR UPDATE OF a SKIP LOCKED
		LIMIT 1`, now).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycle.Candidate{}, false, nil
	}
	if err != nil {
		return lifecycle.Candidate{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET purge_claimed_until=$2 WHERE id=$1`, assetID, now.Add(lease)); err != nil {
		return lifecycle.Candidate{}, false, err
	}
	var objectKey, stagingKey string
	if err := tx.QueryRowContext(ctx, `SELECT a.object_key,COALESCE(u.staging_object_key,'') FROM assets a LEFT JOIN upload_sessions u ON u.asset_id=a.id WHERE a.id=$1`, assetID).Scan(&objectKey, &stagingKey); err != nil {
		return lifecycle.Candidate{}, false, err
	}
	keys := []string{stagingKey, objectKey}
	rows, err := tx.QueryContext(ctx, `SELECT object_key FROM asset_derivatives WHERE asset_id=$1`, assetID)
	if err != nil {
		return lifecycle.Candidate{}, false, err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return lifecycle.Candidate{}, false, err
		}
		keys = append(keys, key)
	}
	if err := rows.Close(); err != nil {
		return lifecycle.Candidate{}, false, err
	}
	for _, variant := range []string{"small", "medium", "large"} {
		keys = append(keys, path.Join(path.Dir(objectKey), "derivatives", variant+".jpg"))
	}
	for attempt := 1; attempt <= 5; attempt++ {
		for _, variant := range []string{"small", "medium", "large"} {
			keys = append(keys, path.Join(path.Dir(objectKey), "derivatives", "attempt-"+strconv.Itoa(attempt), variant+".jpg"))
		}
	}
	if err := tx.Commit(); err != nil {
		return lifecycle.Candidate{}, false, err
	}
	return lifecycle.Candidate{AssetID: assetID, Keys: uniqueStrings(keys)}, true, nil
}

func (s *Store) CompletePurge(ctx context.Context, assetID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET purged_at=$2,purge_claimed_until=NULL,purge_error='',updated_at=$2 WHERE id=$1 AND purged_at IS NULL`, assetID, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) RetryPurge(ctx context.Context, assetID, details string, nextAttempt, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET purge_error=$2,purge_next_attempt_at=$3,purge_claimed_until=NULL,updated_at=$4 WHERE id=$1 AND purged_at IS NULL`, assetID, details, nextAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteExpiredPurge(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, assets.ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM assets
		WHERE id IN (
			SELECT id
			FROM assets
			WHERE purged_at < $1
			ORDER BY purged_at,id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)`, before, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (s *Store) ClaimPendingProcessing(ctx context.Context, now time.Time, lease time.Duration) (assets.Asset, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Asset{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE assets
		SET processing_status='failed',
		    processing_error='processing lease expired after maximum attempts',
		    processing_next_attempt_at=NULL,
		    processing_claimed_until=NULL,
		    updated_at=$1
		WHERE upload_status='completed'
		  AND scan_status='clean'
		  AND processing_status='pending'
		  AND processing_attempts >= 5
		  AND deleted_at IS NULL
		  AND purged_at IS NULL
		  AND (processing_claimed_until IS NULL OR processing_claimed_until < $1)`, now); err != nil {
		return assets.Asset{}, false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM assets
		WHERE upload_status='completed'
		  AND scan_status='clean'
		  AND processing_status='pending'
		  AND processing_attempts < 5
		  AND deleted_at IS NULL
		  AND purged_at IS NULL
		  AND (processing_next_attempt_at IS NULL OR processing_next_attempt_at <= $1)
		  AND (processing_claimed_until IS NULL OR processing_claimed_until < $1)
		ORDER BY COALESCE(processing_next_attempt_at, updated_at), updated_at
		FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return assets.Asset{}, false, err
		}
		return assets.Asset{}, false, nil
	}
	if err != nil {
		return assets.Asset{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET processing_attempts=processing_attempts+1,processing_claimed_until=$2 WHERE id=$1 AND purged_at IS NULL`, id, now.Add(lease)); err != nil {
		return assets.Asset{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return assets.Asset{}, false, err
	}
	asset, err := s.GetAsset(ctx, id)
	return asset, err == nil, err
}

func (s *Store) CompleteProcessing(ctx context.Context, assetID, expectedETag string, expectedAttempt int, derivatives []assets.Derivative, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE assets SET processing_status='ready',processing_error='',processing_next_attempt_at=NULL,processing_claimed_until=NULL,updated_at=$4 WHERE id=$1 AND processing_status='pending' AND etag=$2 AND processing_attempts=$3 AND deleted_at IS NULL AND purged_at IS NULL`, assetID, expectedETag, expectedAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		_ = tx.Rollback()
		committed, readErr := s.processingCommitted(ctx, assetID, expectedETag, expectedAttempt, derivatives)
		if readErr != nil {
			return readErr
		}
		if committed {
			return nil
		}
		if _, getErr := s.GetAsset(ctx, assetID); errors.Is(getErr, assets.ErrNotFound) {
			return assets.ErrNotFound
		}
		return assets.ErrConflict
	}
	for _, value := range derivatives {
		_, err = tx.ExecContext(ctx, `INSERT INTO asset_derivatives(asset_id,variant,object_key,mime_type,width,height,size_bytes,etag,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(asset_id,variant) DO UPDATE SET object_key=EXCLUDED.object_key,mime_type=EXCLUDED.mime_type,width=EXCLUDED.width,height=EXCLUDED.height,size_bytes=EXCLUDED.size_bytes,etag=EXCLUDED.etag,created_at=EXCLUDED.created_at`, assetID, value.Variant, value.ObjectKey, value.MIMEType, value.Width, value.Height, value.SizeBytes, value.ETag, value.CreatedAt)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		committed, readErr := s.processingCommitted(ctx, assetID, expectedETag, expectedAttempt, derivatives)
		if readErr != nil {
			return errors.Join(assets.ErrCommitOutcomeUnknown, err, readErr)
		}
		if committed {
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) FailProcessing(ctx context.Context, assetID, expectedETag string, expectedAttempt int, details string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET processing_status='failed',processing_error=$4,processing_next_attempt_at=NULL,processing_claimed_until=NULL,updated_at=$5 WHERE id=$1 AND processing_status='pending' AND etag=$2 AND processing_attempts=$3 AND deleted_at IS NULL AND purged_at IS NULL`, assetID, expectedETag, expectedAttempt, details, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) ScheduleProcessingRetry(ctx context.Context, assetID, expectedETag string, expectedAttempt int, details string, nextAttempt, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET processing_error=$4,processing_next_attempt_at=$5,processing_claimed_until=NULL,updated_at=$6 WHERE id=$1 AND processing_status='pending' AND etag=$2 AND processing_attempts=$3 AND deleted_at IS NULL AND purged_at IS NULL`, assetID, expectedETag, expectedAttempt, details, nextAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) processingCommitted(ctx context.Context, assetID, expectedETag string, expectedAttempt int, expected []assets.Derivative) (bool, error) {
	var status, etag string
	var attempt int
	err := s.db.QueryRowContext(ctx, `SELECT processing_status,etag,processing_attempts FROM assets WHERE id=$1 AND purged_at IS NULL`, assetID).Scan(&status, &etag, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != string(assets.ProcessingReady) || etag != expectedETag || attempt != expectedAttempt {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT asset_id,variant,object_key,mime_type,width,height,size_bytes,etag,created_at FROM asset_derivatives WHERE asset_id=$1`, assetID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	expectedByVariant := make(map[string]assets.Derivative, len(expected))
	for _, derivative := range expected {
		expectedByVariant[derivative.Variant] = derivative
	}
	matched := 0
	for rows.Next() {
		var derivative assets.Derivative
		if err := rows.Scan(&derivative.AssetID, &derivative.Variant, &derivative.ObjectKey, &derivative.MIMEType, &derivative.Width, &derivative.Height, &derivative.SizeBytes, &derivative.ETag, &derivative.CreatedAt); err != nil {
			return false, err
		}
		want, ok := expectedByVariant[derivative.Variant]
		if !ok || derivative.AssetID != want.AssetID || derivative.ObjectKey != want.ObjectKey || derivative.MIMEType != want.MIMEType || derivative.Width != want.Width || derivative.Height != want.Height || derivative.SizeBytes != want.SizeBytes || derivative.ETag != want.ETag {
			return false, nil
		}
		matched++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return matched == len(expectedByVariant), nil
}

func (s *Store) GetDerivative(ctx context.Context, assetID, variant string) (assets.Derivative, error) {
	var value assets.Derivative
	err := s.db.QueryRowContext(ctx, `SELECT d.asset_id,d.variant,d.object_key,d.mime_type,d.width,d.height,d.size_bytes,d.etag,d.created_at FROM asset_derivatives d JOIN assets a ON a.id=d.asset_id WHERE d.asset_id=$1 AND d.variant=$2 AND a.purged_at IS NULL`, assetID, variant).Scan(&value.AssetID, &value.Variant, &value.ObjectKey, &value.MIMEType, &value.Width, &value.Height, &value.SizeBytes, &value.ETag, &value.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.Derivative{}, assets.ErrNotFound
	}
	return value, err
}
