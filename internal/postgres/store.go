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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/derivativequeue"
	"hhc/asset-api/internal/lifecycle"
	"hhc/asset-api/internal/retention"

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
	row := s.db.QueryRowContext(ctx, `SELECT id,namespace,owner_service,owner_type,owner_id,purpose,locale,original_file_name,object_key,expected_mime_type,detected_mime_type,size_bytes,checksum_sha256,etag,upload_status,scan_status,scan_details,scan_signature_version,scan_failure_category,processing_status,visibility,created_at,updated_at,COALESCE(deleted_at,'0001-01-01'::timestamptz),scan_attempts,scan_event_id,processing_attempts,processing_error,COALESCE(processing_next_attempt_at,'0001-01-01'::timestamptz),COALESCE(processing_claimed_until,'0001-01-01'::timestamptz) FROM assets WHERE id=$1 AND purged_at IS NULL`, id)
	var value assets.Asset
	err := row.Scan(&value.ID, &value.Namespace, &value.OwnerService, &value.OwnerType, &value.OwnerID, &value.Purpose, &value.Locale, &value.OriginalFileName, &value.ObjectKey, &value.ExpectedMIMEType, &value.DetectedMIMEType, &value.SizeBytes, &value.ChecksumSHA256, &value.ETag, &value.UploadStatus, &value.ScanStatus, &value.ScanDetails, &value.ScanSignature, &value.ScanFailure, &value.ProcessingStatus, &value.Visibility, &value.CreatedAt, &value.UpdatedAt, &value.DeletedAt, &value.ScanAttempts, &value.ScanEventID, &value.ProcessingAttempts, &value.ProcessingError, &value.ProcessingNextAt, &value.ProcessingClaimedUntil)
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

func (s *Store) ClaimDerivativeRequest(ctx context.Context, now time.Time, lease time.Duration) (derivativequeue.Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return derivativequeue.Request{}, false, err
	}
	defer tx.Rollback()
	var request derivativequeue.Request
	err = tx.QueryRowContext(ctx, `
		SELECT event_id,asset_id,asset_etag,attempts,created_at
		FROM asset_derivative_outbox
		WHERE delivered_at IS NULL AND available_at <= $1
		  AND (claimed_until IS NULL OR claimed_until < $1)
		ORDER BY available_at,created_at
		FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&request.EventID, &request.AssetID, &request.ETag, &request.Attempts, &request.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return derivativequeue.Request{}, false, nil
	}
	if err != nil {
		return derivativequeue.Request{}, false, err
	}
	request.Attempts++
	if _, err := tx.ExecContext(ctx, `UPDATE asset_derivative_outbox SET attempts=$2,claimed_until=$3 WHERE event_id=$1`, request.EventID, request.Attempts, now.Add(lease)); err != nil {
		return derivativequeue.Request{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return derivativequeue.Request{}, false, err
	}
	return request, true, nil
}

func (s *Store) MarkDerivativeRequestDelivered(ctx context.Context, eventID string, expectedAttempt int, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE asset_derivative_outbox SET delivered_at=$3,claimed_until=NULL,last_error='' WHERE event_id=$1 AND attempts=$2 AND delivered_at IS NULL`, eventID, expectedAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return s.derivativeRequestTransitionError(ctx, eventID)
	}
	return nil
}

func (s *Store) ScheduleDerivativeRequestRetry(ctx context.Context, eventID string, expectedAttempt int, details string, nextAttempt, _ time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE asset_derivative_outbox SET available_at=$3,claimed_until=NULL,last_error=$4 WHERE event_id=$1 AND attempts=$2 AND delivered_at IS NULL`, eventID, expectedAttempt, nextAttempt, details)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return s.derivativeRequestTransitionError(ctx, eventID)
	}
	return nil
}

func (s *Store) derivativeRequestTransitionError(ctx context.Context, eventID string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_derivative_outbox WHERE event_id=$1)`, eventID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return assets.ErrConflict
	}
	return assets.ErrNotFound
}

func (s *Store) RecordDerivativePoison(ctx context.Context, poison assets.DerivativePoison, now time.Time) (bool, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO asset_derivative_poison_events(poison_id,event_id,asset_id,asset_etag,reason,details,dequeue_count,source_message_id,body_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(poison_id) DO NOTHING`, poison.PoisonID, poison.EventID, poison.AssetID, poison.ETag, poison.Reason, poison.Details, poison.DequeueCount, poison.SourceMessageID, poison.BodySHA256, now)
	if err != nil {
		return false, err
	}
	var shouldForward bool
	err = s.db.QueryRowContext(ctx, `SELECT forwarded_at IS NULL FROM asset_derivative_poison_events WHERE poison_id=$1`, poison.PoisonID).Scan(&shouldForward)
	return shouldForward, err
}

func (s *Store) FailDerivativeToPoison(ctx context.Context, failure assets.ProcessingFailure, poison assets.DerivativePoison, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE assets SET processing_status='failed',processing_error=$4,processing_next_attempt_at=NULL,processing_claimed_until=NULL,updated_at=$5 WHERE id=$1 AND processing_status='pending' AND etag=$2 AND processing_attempts=$3 AND deleted_at IS NULL AND purged_at IS NULL`, failure.AssetID, failure.ETag, failure.ExpectedAttempt, failure.Details, now)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var status, etag string
		if err := tx.QueryRowContext(ctx, `SELECT processing_status,etag FROM assets WHERE id=$1 AND purged_at IS NULL`, failure.AssetID).Scan(&status, &etag); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, assets.ErrNotFound
			}
			return false, err
		}
		if status != string(assets.ProcessingFailed) || etag != failure.ETag {
			return false, assets.ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asset_derivative_poison_events(poison_id,event_id,asset_id,asset_etag,reason,details,dequeue_count,source_message_id,body_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(poison_id) DO NOTHING`, poison.PoisonID, poison.EventID, poison.AssetID, poison.ETag, poison.Reason, poison.Details, poison.DequeueCount, poison.SourceMessageID, poison.BodySHA256, now); err != nil {
		return false, err
	}
	var shouldForward bool
	if err := tx.QueryRowContext(ctx, `SELECT forwarded_at IS NULL FROM asset_derivative_poison_events WHERE poison_id=$1`, poison.PoisonID).Scan(&shouldForward); err != nil {
		return false, err
	}
	return shouldForward, tx.Commit()
}

func (s *Store) MarkDerivativePoisonForwarded(ctx context.Context, poisonID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE asset_derivative_poison_events SET forwarded_at=COALESCE(forwarded_at,$2) WHERE poison_id=$1`, poisonID, now)
	return err
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
	createCollectionOperation            = "create_collection"
	renameCollectionOperation            = "rename_collection"
	deleteCollectionOperation            = "delete_collection"
	addCollectionACLOperation            = "add_collection_acl"
	revokeCollectionACLOperation         = "revoke_collection_acl"
	addCollectionItemOperation           = "add_collection_item"
	deleteCollectionItemOperation        = "delete_collection_item"
	deleteCollectionItemsOperation       = "delete_collection_items"
	renameCollectionItemOperation        = "rename_collection_item"
	setCollectionItemsRetentionOperation = "set_collection_items_retention"
	updateCollectionRetentionOperation   = "update_collection_retention"
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
	value := assets.Collection{ID: newStoreID(), Namespace: input.Namespace, Name: input.Name, Revision: 1, RetentionDays: 14, CreatedByService: input.CallerService, CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_collections(id,namespace,name,revision,retention_days,created_by_service,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7)`, value.ID, value.Namespace, value.Name, value.Revision, value.RetentionDays, value.CreatedByService, now)
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
	nextRevision := value.Revision + 1
	rows, err := tx.QueryContext(ctx, `SELECT id FROM asset_collection_items WHERE collection_id=$1 AND deleted_revision IS NULL ORDER BY id`, input.CollectionID)
	if err != nil {
		return assets.Collection{}, err
	}
	var itemIDs []string
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			rows.Close()
			return assets.Collection{}, err
		}
		itemIDs = append(itemIDs, itemID)
	}
	if err := finishRows(rows); err != nil {
		return assets.Collection{}, err
	}
	deleted, err := s.deleteCollectionItems(ctx, tx, input.CollectionID, input.CallerService, itemIDs, now, false)
	if err != nil {
		return assets.Collection{}, err
	}
	value = deleted.collection
	value.Revision, value.UpdatedAt, value.DeletedAt = nextRevision, now, now
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
		ActorUserID  string
	}{input.CollectionID, input.SubjectType, input.SubjectID, input.Permission, input.ActorUserID})
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
	if err := insertCollectionACLAudit(ctx, tx, "add", acl, input.ActorUserID, input.RequestID, now); err != nil {
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
	fingerprint := mutationFingerprint(struct{ CollectionID, ACLID, ActorUserID string }{input.CollectionID, input.ACLID, input.ActorUserID})
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
	if err := insertCollectionACLAudit(ctx, tx, "revoke", acl, input.ActorUserID, input.RequestID, now); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	value := assets.CollectionACLMutation{Collection: collection, ACL: acl}
	if err := finishMutation(ctx, tx, input.CallerService, revokeCollectionACLOperation, input.IdempotencyKey, value); err != nil {
		return assets.CollectionACLMutation{}, err
	}
	return value, nil
}

func insertCollectionACLAudit(ctx context.Context, tx *sql.Tx, action string, acl assets.CollectionACL, actorUserID, requestID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO asset_collection_acl_audit(id,collection_id,acl_id,action,subject_type,subject_id,actor_user_id,request_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, newStoreID(), acl.CollectionID, acl.ID, action, acl.SubjectType, acl.SubjectID, actorUserID, requestID, now)
	return err
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
	collection, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	var namespace, ownerService, mimeType, etag string
	var uploadStatus assets.UploadStatus
	var scanStatus assets.ScanStatus
	var processingStatus assets.ProcessingStatus
	var sizeBytes int64
	err = tx.QueryRowContext(ctx, `SELECT namespace,owner_service,upload_status,scan_status,processing_status,detected_mime_type,size_bytes,etag FROM assets WHERE id=$1 AND deleted_at IS NULL AND purged_at IS NULL FOR UPDATE`, input.AssetID).Scan(&namespace, &ownerService, &uploadStatus, &scanStatus, &processingStatus, &mimeType, &sizeBytes, &etag)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionItemMutation{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if ownerService != input.CallerService {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	if uploadStatus != assets.UploadCompleted || scanStatus != assets.ScanClean || (processingStatus != assets.ProcessingReady && processingStatus != assets.ProcessingNotRequired) {
		return assets.CollectionItemMutation{}, assets.ErrConflict
	}
	if namespace != collection.Namespace {
		return assets.CollectionItemMutation{}, assets.ErrConflict
	}
	collection.Revision, collection.UpdatedAt = collection.Revision+1, now
	item := assets.CollectionItem{ID: newStoreID(), CollectionID: collection.ID, AssetID: input.AssetID, RemoteItemID: input.RemoteItemID, DisplayName: input.DisplayName, SourceRevision: input.SourceRevision, CreatedRevision: collection.Revision, RetentionExempt: false, UpdatedRevision: collection.Revision, MIMEType: mimeType, SizeBytes: sizeBytes, ETag: etag, CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`, item.ID, item.CollectionID, item.AssetID, item.RemoteItemID, item.DisplayName, item.SourceRevision, item.CreatedRevision, item.RetentionExempt, item.UpdatedRevision, now)
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
	deleted, err := s.deleteCollectionItems(ctx, tx, input.CollectionID, input.CallerService, []string{input.ItemID}, now, false)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	value := assets.CollectionItemMutation{Collection: deleted.collection}
	if len(deleted.items) == 1 {
		value.Item, value.Tombstone = deleted.items[0], deleted.tombstones[0]
	}
	if err := finishMutation(ctx, tx, input.CallerService, deleteCollectionItemOperation, input.IdempotencyKey, value); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	return value, nil
}

func (s *Store) DeleteCollectionItems(ctx context.Context, input assets.DeleteCollectionItemsInput, now time.Time) (assets.DeleteCollectionItemsResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.DeleteCollectionItemsResult{}, err
	}
	defer tx.Rollback()
	itemIDs := uniqueSortedStrings(input.ItemIDs)
	fingerprint := mutationFingerprint(struct {
		CollectionID string
		ItemIDs      []string
	}{input.CollectionID, itemIDs})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, deleteCollectionItemsOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.DeleteCollectionItemsResult{}, err
	}
	if !claimed {
		var value assets.DeleteCollectionItemsResult
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.DeleteCollectionItemsResult{}, err
		}
		return value, nil
	}
	deleted, err := s.deleteCollectionItems(ctx, tx, input.CollectionID, input.CallerService, itemIDs, now, false)
	if err != nil {
		return assets.DeleteCollectionItemsResult{}, err
	}
	if err := finishMutation(ctx, tx, input.CallerService, deleteCollectionItemsOperation, input.IdempotencyKey, deleted.result); err != nil {
		return assets.DeleteCollectionItemsResult{}, err
	}
	return deleted.result, nil
}

type collectionItemsDeletion struct {
	collection assets.Collection
	items      []assets.CollectionItem
	tombstones []assets.CollectionTombstone
	result     assets.DeleteCollectionItemsResult
}

func (s *Store) deleteCollectionItems(ctx context.Context, tx *sql.Tx, collectionID, callerService string, itemIDs []string, now time.Time, respectRetentionExempt bool) (collectionItemsDeletion, error) {
	itemIDs = uniqueSortedStrings(itemIDs)
	collection, err := lockManagedCollection(ctx, tx, collectionID, callerService)
	if err != nil {
		return collectionItemsDeletion{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id,collection_id,COALESCE(asset_id,''),remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at
		FROM asset_collection_items
		WHERE collection_id=$1 AND id=ANY($2::text[]) AND deleted_revision IS NULL
		ORDER BY id FOR UPDATE`, collectionID, itemIDs)
	if err != nil {
		return collectionItemsDeletion{}, err
	}
	deletion := collectionItemsDeletion{collection: collection}
	var items []assets.CollectionItem
	for rows.Next() {
		var item assets.CollectionItem
		if err := rows.Scan(&item.ID, &item.CollectionID, &item.AssetID, &item.RemoteItemID, &item.DisplayName, &item.SourceRevision, &item.CreatedRevision, &item.RetentionExempt, &item.UpdatedRevision, &item.CreatedAt, &item.UpdatedAt); err != nil {
			rows.Close()
			return collectionItemsDeletion{}, err
		}
		if respectRetentionExempt && item.RetentionExempt {
			deletion.result.ExemptSkipped++
			continue
		}
		if respectRetentionExempt && item.CreatedAt.Add(time.Duration(collection.RetentionDays)*24*time.Hour).After(now) {
			continue
		}
		items = append(items, item)
	}
	if err := finishRows(rows); err != nil {
		return collectionItemsDeletion{}, err
	}
	deletion.collection = collection
	deletion.items = items
	deletion.result.Deleted = len(items)
	deletion.result.AlreadyRemoved = len(itemIDs) - len(items) - deletion.result.ExemptSkipped
	if len(items) == 0 {
		return deletion, nil
	}
	deletion.collection.Revision++
	deletion.collection.UpdatedAt = now
	assetIDs := make([]string, 0, len(items))
	deletedIDs := make([]string, 0, len(items))
	deletion.tombstones = make([]assets.CollectionTombstone, 0, len(items))
	for index := range deletion.items {
		item := &deletion.items[index]
		item.DeletedRevision, item.DeletedAt = deletion.collection.Revision, now
		deletedIDs = append(deletedIDs, item.ID)
		assetIDs = append(assetIDs, item.AssetID)
		deletion.tombstones = append(deletion.tombstones, assets.CollectionTombstone{ID: item.ID, RemoteItemID: item.RemoteItemID, DeletedRevision: item.DeletedRevision, DeletedAt: now})
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET deleted_revision=$3,deleted_at=$4 WHERE collection_id=$1 AND id=ANY($2::text[]) AND deleted_revision IS NULL`, collectionID, deletedIDs, deletion.collection.Revision, now); err != nil {
		return collectionItemsDeletion{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collectionID, deletion.collection.Revision, now); err != nil {
		return collectionItemsDeletion{}, err
	}
	assetIDs = uniqueSortedStrings(assetIDs)
	if len(assetIDs) > 0 {
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM assets
			WHERE id=ANY($1::text[])
			  AND namespace='line.group.media-sync'
			  AND owner_service='hhc-line-function-bot'
			  AND owner_type='media_sync_ingest'
			  AND deleted_at IS NULL AND purged_at IS NULL
			ORDER BY id FOR UPDATE`, assetIDs)
		if err != nil {
			return collectionItemsDeletion{}, err
		}
		var candidateIDs []string
		for rows.Next() {
			var assetID string
			if err := rows.Scan(&assetID); err != nil {
				rows.Close()
				return collectionItemsDeletion{}, err
			}
			candidateIDs = append(candidateIDs, assetID)
		}
		if err := finishRows(rows); err != nil {
			return collectionItemsDeletion{}, err
		}
		if len(candidateIDs) == 0 {
			return deletion, nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE assets a SET deleted_at=$2,updated_at=$2
			WHERE a.id=ANY($1::text[])
			  AND a.namespace='line.group.media-sync'
			  AND a.owner_service='hhc-line-function-bot'
			  AND a.owner_type='media_sync_ingest'
			  AND a.deleted_at IS NULL AND a.purged_at IS NULL
			  AND NOT EXISTS (
			    SELECT 1 FROM asset_collection_items i
			    WHERE i.asset_id=a.id AND i.deleted_revision IS NULL
			  )`, candidateIDs, now); err != nil {
			return collectionItemsDeletion{}, err
		}
	}
	return deletion, nil
}

func (s *Store) RenameCollectionItem(ctx context.Context, input assets.RenameCollectionItemInput, now time.Time) (assets.ManagedCollectionItem, error) {
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if !validCollectionItemDisplayName(input.DisplayName) {
		return assets.ManagedCollectionItem{}, assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.ManagedCollectionItem{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct{ CollectionID, ItemID, DisplayName string }{input.CollectionID, input.ItemID, input.DisplayName})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, renameCollectionItemOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.ManagedCollectionItem{}, err
	}
	if !claimed {
		var value assets.ManagedCollectionItem
		if err := json.Unmarshal(replay, &value); err != nil {
			return assets.ManagedCollectionItem{}, err
		}
		return value, nil
	}
	collection, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.ManagedCollectionItem{}, err
	}
	var item assets.CollectionItem
	err = tx.QueryRowContext(ctx, `
		SELECT i.id,i.collection_id,i.display_name,i.source_revision,i.created_revision,i.retention_exempt,i.updated_revision,i.created_at,i.updated_at,
		       COALESCE(a.detected_mime_type,''),COALESCE(a.size_bytes,0),COALESCE(a.etag,'')
		FROM asset_collection_items i
		LEFT JOIN assets a ON a.id=i.asset_id
		WHERE i.id=$1 AND i.collection_id=$2 AND i.deleted_revision IS NULL
		FOR UPDATE OF i`, input.ItemID, input.CollectionID).Scan(&item.ID, &item.CollectionID, &item.DisplayName, &item.SourceRevision, &item.CreatedRevision, &item.RetentionExempt, &item.UpdatedRevision, &item.CreatedAt, &item.UpdatedAt, &item.MIMEType, &item.SizeBytes, &item.ETag)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ManagedCollectionItem{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.ManagedCollectionItem{}, err
	}
	if !strings.EqualFold(path.Ext(item.DisplayName), path.Ext(input.DisplayName)) {
		return assets.ManagedCollectionItem{}, assets.ErrInvalidInput
	}
	if item.DisplayName != input.DisplayName {
		collection.Revision, collection.UpdatedAt = collection.Revision+1, now
		item.DisplayName, item.UpdatedRevision, item.UpdatedAt = input.DisplayName, collection.Revision, now
		if _, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET display_name=$2,updated_revision=$3,updated_at=$4 WHERE id=$1`, item.ID, item.DisplayName, item.UpdatedRevision, now); err != nil {
			return assets.ManagedCollectionItem{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collection.ID, collection.Revision, now); err != nil {
			return assets.ManagedCollectionItem{}, err
		}
	}
	value := assets.ManagedCollectionItem{ID: item.ID, DisplayName: item.DisplayName, MIMEType: item.MIMEType, SizeBytes: item.SizeBytes, CreatedAt: item.CreatedAt, RetentionExempt: item.RetentionExempt}
	if err := finishMutation(ctx, tx, input.CallerService, renameCollectionItemOperation, input.IdempotencyKey, value); err != nil {
		return assets.ManagedCollectionItem{}, err
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
	err := tx.QueryRowContext(ctx, `SELECT id,namespace,name,revision,retention_days,created_by_service,created_at,updated_at,COALESCE(deleted_at,'0001-01-01'::timestamptz) FROM asset_collections WHERE id=$1 FOR UPDATE`, id).Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.RetentionDays, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt, &value.DeletedAt)
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

func validCollectionItemDisplayName(value string) bool {
	return value != "" && len(value) <= 255 && !strings.ContainsAny(value, "/\\\\") && !strings.ContainsFunc(value, unicode.IsControl)
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

type managedItemCursor struct {
	CreatedAt time.Time `json:"t"`
	LastID    string    `json:"i"`
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
	if subject.UserID == "" {
		return assets.CollectionPage{}, assets.ErrForbidden
	}
	lastID := ""
	if decoded, ok := decodeListCursor(cursor); ok {
		lastID = decoded.LastID
	}
	limit = boundedCollectionLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id,c.namespace,c.name,c.revision,c.retention_days,c.created_by_service,c.created_at,c.updated_at
		FROM asset_collections c
		WHERE c.deleted_at IS NULL AND c.id>$3
		  AND EXISTS (
		    SELECT 1 FROM asset_collection_acl acl
		    WHERE acl.collection_id=c.id AND acl.permission='read' AND acl.revoked_at IS NULL
		      AND ((acl.subject_type='user' AND acl.subject_id=$1)
		        OR (acl.subject_type='role' AND acl.subject_id=ANY($2::text[])))
		  )
		ORDER BY c.id LIMIT $4`, subject.UserID, subject.RoleIDs, lastID, limit+1)
	if err != nil {
		return assets.CollectionPage{}, err
	}
	defer rows.Close()
	page := assets.CollectionPage{Collections: []assets.Collection{}}
	for rows.Next() {
		var value assets.Collection
		if err := rows.Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.RetentionDays, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt); err != nil {
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
	if subject.UserID == "" {
		return assets.Collection{}, assets.ErrForbidden
	}
	value, err := s.getLiveCollection(ctx, id)
	if err != nil {
		return assets.Collection{}, err
	}
	allowed, err := s.hasCollectionACL(ctx, id, subject)
	if err != nil {
		return assets.Collection{}, err
	}
	if !allowed {
		return assets.Collection{}, assets.ErrNotFound
	}
	return value, nil
}

func (s *Store) GetAuthorizedCollectionItem(ctx context.Context, collectionID, itemID string, subject assets.CollectionSubject) (assets.CollectionItem, error) {
	return s.getAuthorizedCollectionItem(ctx, collectionID, itemID, subject)
}

func (s *Store) GetAuthorizedCollectionItemByID(ctx context.Context, itemID string, subject assets.CollectionSubject) (assets.CollectionItem, error) {
	return s.getAuthorizedCollectionItem(ctx, "", itemID, subject)
}

func (s *Store) getAuthorizedCollectionItem(ctx context.Context, collectionID, itemID string, subject assets.CollectionSubject) (assets.CollectionItem, error) {
	var allowed bool
	var id, itemCollectionID, assetID, remoteItemID, displayName, sourceRevision, mimeType, etag sql.NullString
	var createdRevision, updatedRevision, sizeBytes sql.NullInt64
	var retentionExempt sql.NullBool
	var createdAt, updatedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  EXISTS (
		    SELECT 1 FROM asset_collection_acl acl
		    WHERE acl.collection_id=c.id AND acl.permission='read' AND acl.revoked_at IS NULL
		      AND ((acl.subject_type='user' AND acl.subject_id=$4)
		        OR (acl.subject_type='role' AND acl.subject_id=ANY($3::text[])))
		  ) AS allowed,
		  i.id,i.collection_id,i.asset_id,i.remote_item_id,i.display_name,i.source_revision,i.created_revision,i.retention_exempt,i.updated_revision,i.created_at,i.updated_at,
		  a.detected_mime_type,a.size_bytes,a.etag
		FROM asset_collections c
		LEFT JOIN asset_collection_items i
		  ON i.collection_id=c.id AND i.id=$2 AND i.deleted_revision IS NULL
		LEFT JOIN assets a
		  ON a.id=i.asset_id AND a.deleted_at IS NULL AND a.purged_at IS NULL
		 AND a.upload_status='completed' AND a.scan_status='clean'
		 AND a.processing_status IN ('ready','not_required')
		WHERE (($1='' AND i.id IS NOT NULL) OR c.id=$1) AND c.deleted_at IS NULL`, collectionID, itemID, subject.RoleIDs, subject.UserID).Scan(
		&allowed, &id, &itemCollectionID, &assetID, &remoteItemID, &displayName, &sourceRevision, &createdRevision, &retentionExempt, &updatedRevision, &createdAt, &updatedAt, &mimeType, &sizeBytes, &etag,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionItem{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionItem{}, err
	}
	if !allowed {
		return assets.CollectionItem{}, assets.ErrNotFound
	}
	if !id.Valid || !itemCollectionID.Valid || !assetID.Valid || !remoteItemID.Valid || !displayName.Valid || !sourceRevision.Valid || !createdRevision.Valid || !retentionExempt.Valid || !updatedRevision.Valid || !createdAt.Valid || !updatedAt.Valid || !mimeType.Valid || !sizeBytes.Valid || !etag.Valid {
		return assets.CollectionItem{}, assets.ErrNotFound
	}
	return assets.CollectionItem{
		ID: id.String, CollectionID: itemCollectionID.String, AssetID: assetID.String,
		RemoteItemID: remoteItemID.String, DisplayName: displayName.String, SourceRevision: sourceRevision.String,
		CreatedRevision: createdRevision.Int64, RetentionExempt: retentionExempt.Bool, UpdatedRevision: updatedRevision.Int64, MIMEType: mimeType.String, SizeBytes: sizeBytes.Int64,
		ETag: etag.String, CreatedAt: createdAt.Time, UpdatedAt: updatedAt.Time,
	}, nil
}

func (s *Store) GetManagedCollectionItem(ctx context.Context, collectionID, itemID, callerService string) (assets.CollectionItem, error) {
	var item assets.CollectionItem
	err := s.db.QueryRowContext(ctx, `
		SELECT i.id,i.collection_id,i.asset_id,i.remote_item_id,i.display_name,i.source_revision,i.created_revision,i.retention_exempt,i.updated_revision,i.created_at,i.updated_at,
		       a.detected_mime_type,a.size_bytes,a.etag
		FROM asset_collections c
		JOIN asset_collection_items i ON i.collection_id=c.id AND i.id=$2 AND i.deleted_revision IS NULL
		JOIN assets a ON a.id=i.asset_id AND a.deleted_at IS NULL AND a.purged_at IS NULL
		 AND a.upload_status='completed' AND a.scan_status='clean' AND a.processing_status IN ('ready','not_required')
		WHERE c.id=$1 AND c.namespace='line.group.media-sync' AND c.created_by_service=$3 AND c.deleted_at IS NULL`, collectionID, itemID, callerService).Scan(
		&item.ID, &item.CollectionID, &item.AssetID, &item.RemoteItemID, &item.DisplayName, &item.SourceRevision, &item.CreatedRevision,
		&item.RetentionExempt, &item.UpdatedRevision, &item.CreatedAt, &item.UpdatedAt, &item.MIMEType, &item.SizeBytes, &item.ETag,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionItem{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionItem{}, err
	}
	return item, nil
}

func (s *Store) CreateContentTicket(ctx context.Context, ticket assets.ContentTicket, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_content_tickets WHERE expires_at <= $1`, now); err != nil {
		return err
	}
	accessMode := ticket.AccessMode
	if accessMode == "" {
		accessMode = "reader"
	}
	if accessMode != "reader" && accessMode != "manager" {
		return assets.ErrNotFound
	}
	roleIDs := ticket.RoleIDs
	if roleIDs == nil {
		roleIDs = []string{}
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO asset_content_tickets(
		  token_hash,collection_id,collection_item_id,asset_etag,user_id,role_ids,access_mode,expires_at,created_at
		)
		SELECT $1,c.id,i.id,a.etag,$5,$6::text[],$7,$8::timestamptz,$9::timestamptz
		FROM asset_collections c
		JOIN asset_collection_items i
		  ON i.collection_id=c.id AND i.id=$3 AND i.deleted_revision IS NULL
		JOIN assets a
		  ON a.id=i.asset_id AND a.deleted_at IS NULL AND a.purged_at IS NULL
		 AND a.upload_status='completed' AND a.scan_status='clean'
		 AND a.processing_status IN ('ready','not_required') AND a.etag=$4
		WHERE c.id=$2 AND c.deleted_at IS NULL
		  AND $8::timestamptz>$10::timestamptz AND $8::timestamptz<=$10::timestamptz+interval '5 minutes'
		  AND ($7='manager' OR ($7='reader' AND EXISTS (
		    SELECT 1 FROM asset_collection_acl acl
		    WHERE acl.collection_id=c.id AND acl.permission='read' AND acl.revoked_at IS NULL
		      AND ((acl.subject_type='user' AND acl.subject_id=$5)
		        OR (acl.subject_type='role' AND acl.subject_id=ANY($6::text[])))
		  )))`, ticket.TokenHash, ticket.CollectionID, ticket.CollectionItemID, ticket.AssetETag,
		ticket.UserID, roleIDs, accessMode, ticket.ExpiresAt, ticket.CreatedAt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		if err := tx.Commit(); err != nil {
			return err
		}
		return assets.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) RedeemContentTicket(ctx context.Context, tokenHash string, now time.Time) (assets.Asset, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Asset{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_content_tickets WHERE expires_at <= $1`, now); err != nil {
		return assets.Asset{}, err
	}
	var asset assets.Asset
	err = tx.QueryRowContext(ctx, `
		SELECT a.id,a.namespace,a.object_key,i.display_name,a.detected_mime_type,
		       a.size_bytes,a.etag,a.upload_status,a.scan_status,a.processing_status,
		       a.visibility,a.updated_at
		FROM asset_content_tickets t
		JOIN asset_collections c ON c.id=t.collection_id AND c.deleted_at IS NULL
		JOIN asset_collection_items i
		  ON i.id=t.collection_item_id AND i.collection_id=t.collection_id AND i.deleted_revision IS NULL
		JOIN assets a
		  ON a.id=i.asset_id AND a.deleted_at IS NULL AND a.purged_at IS NULL
		 AND a.upload_status='completed' AND a.scan_status='clean'
		 AND a.processing_status IN ('ready','not_required') AND a.etag=t.asset_etag
		WHERE t.token_hash=$1 AND t.expires_at>$2
		  AND (t.access_mode='manager' OR (t.access_mode='reader' AND EXISTS (
		    SELECT 1 FROM asset_collection_acl acl
		    WHERE acl.collection_id=c.id AND acl.permission='read' AND acl.revoked_at IS NULL
		      AND ((acl.subject_type='user' AND acl.subject_id=t.user_id)
		        OR (acl.subject_type='role' AND acl.subject_id=ANY(t.role_ids)))
		  )))`, tokenHash, now).Scan(
		&asset.ID, &asset.Namespace, &asset.ObjectKey, &asset.OriginalFileName, &asset.DetectedMIMEType,
		&asset.SizeBytes, &asset.ETag, &asset.UploadStatus, &asset.ScanStatus, &asset.ProcessingStatus,
		&asset.Visibility, &asset.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return assets.Asset{}, err
		}
		return assets.Asset{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.Asset{}, err
	}
	if err := tx.Commit(); err != nil {
		return assets.Asset{}, err
	}
	return asset, nil
}

func (s *Store) ListManagedCollections(ctx context.Context, callerService, cursor string, limit int) (assets.ManagedCollectionPage, error) {
	lastID := ""
	if decoded, ok := decodeListCursor(cursor); ok {
		lastID = decoded.LastID
	}
	limit = boundedCollectionLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id,namespace,name,revision,retention_days,created_by_service,created_at,updated_at FROM asset_collections WHERE created_by_service=$1 AND deleted_at IS NULL AND id>$2 ORDER BY id LIMIT $3`, callerService, lastID, limit+1)
	if err != nil {
		return assets.ManagedCollectionPage{}, err
	}
	var collections []assets.Collection
	for rows.Next() {
		var value assets.Collection
		if err := rows.Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.RetentionDays, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt); err != nil {
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

func (s *Store) ListManagedCollectionItems(ctx context.Context, collectionID, callerService, query, cursor string, limit int) (assets.ManagedCollectionItemPage, error) {
	last, ok := decodeManagedItemCursor(cursor)
	if !ok {
		return assets.ManagedCollectionItemPage{}, assets.ErrInvalidInput
	}
	var owned bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_collections WHERE id=$1 AND namespace='line.group.media-sync' AND created_by_service=$2 AND deleted_at IS NULL)`, collectionID, callerService).Scan(&owned); err != nil {
		return assets.ManagedCollectionItemPage{}, err
	}
	if !owned {
		return assets.ManagedCollectionItemPage{}, assets.ErrNotFound
	}
	limit = boundedCollectionLimit(limit)
	query = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id,i.display_name,COALESCE(a.detected_mime_type,''),COALESCE(a.size_bytes,0),i.created_at,i.retention_exempt
		FROM asset_collections c
		JOIN asset_collection_items i ON i.collection_id=c.id
		LEFT JOIN assets a ON a.id=i.asset_id
		WHERE c.id=$1 AND c.namespace='line.group.media-sync' AND c.created_by_service=$2 AND c.deleted_at IS NULL
		  AND i.deleted_revision IS NULL
		  AND i.display_name ILIKE '%' || $3 || '%' ESCAPE '\'
		  AND ($4::timestamptz='0001-01-01'::timestamptz OR i.created_at<$4 OR (i.created_at=$4 AND i.id<$5))
		ORDER BY i.created_at DESC,i.id DESC LIMIT $6`, collectionID, callerService, query, last.CreatedAt, last.LastID, limit+1)
	if err != nil {
		return assets.ManagedCollectionItemPage{}, err
	}
	defer rows.Close()
	page := assets.ManagedCollectionItemPage{Items: []assets.ManagedCollectionItem{}}
	for rows.Next() {
		var item assets.ManagedCollectionItem
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.MIMEType, &item.SizeBytes, &item.CreatedAt, &item.RetentionExempt); err != nil {
			return assets.ManagedCollectionItemPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return assets.ManagedCollectionItemPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		page.HasMore = true
		page.Cursor = encodeManagedItemCursor(managedItemCursor{CreatedAt: last.CreatedAt, LastID: last.ID})
	}
	return page, nil
}

func (s *Store) UpdateCollectionRetention(ctx context.Context, input assets.UpdateCollectionRetentionInput, now time.Time) (assets.Collection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Collection{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct {
		CollectionID  string
		RetentionDays int
	}{input.CollectionID, input.RetentionDays})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, updateCollectionRetentionOperation, input.IdempotencyKey, fingerprint, now)
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
	value.RetentionDays, value.UpdatedAt = input.RetentionDays, now
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET retention_days=$2,updated_at=$3 WHERE id=$1`, value.ID, value.RetentionDays, now); err != nil {
		return assets.Collection{}, err
	}
	if err := finishMutation(ctx, tx, input.CallerService, updateCollectionRetentionOperation, input.IdempotencyKey, value); err != nil {
		return assets.Collection{}, err
	}
	return value, nil
}

func (s *Store) SetCollectionItemsRetention(ctx context.Context, input assets.SetCollectionItemsRetentionInput, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	itemIDs := uniqueSortedStrings(input.ItemIDs)
	fingerprint := mutationFingerprint(struct {
		CollectionID    string
		ItemIDs         []string
		RetentionExempt bool
	}{input.CollectionID, itemIDs, input.RetentionExempt})
	_, claimed, err := claimMutation(ctx, tx, input.CallerService, setCollectionItemsRetentionOperation, input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if _, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM asset_collection_items WHERE collection_id=$1 AND id=ANY($2::text[]) AND deleted_revision IS NULL ORDER BY id FOR UPDATE`, input.CollectionID, itemIDs)
	if err != nil {
		return err
	}
	var activeIDs []string
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			rows.Close()
			return err
		}
		activeIDs = append(activeIDs, itemID)
	}
	if err := finishRows(rows); err != nil {
		return err
	}
	if len(activeIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET retention_exempt=$2,updated_at=$3 WHERE id=ANY($1::text[]) AND deleted_revision IS NULL`, activeIDs, input.RetentionExempt, now); err != nil {
			return err
		}
	}
	return finishMutation(ctx, tx, input.CallerService, setCollectionItemsRetentionOperation, input.IdempotencyKey, struct{}{})
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
		       i.created_revision,i.retention_exempt,i.updated_revision,i.created_at,i.updated_at,COALESCE(a.detected_mime_type,''),COALESCE(a.size_bytes,0),COALESCE(a.etag,'')
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
		if err := rows.Scan(&item.ID, &item.CollectionID, &item.AssetID, &item.RemoteItemID, &item.DisplayName, &item.SourceRevision, &item.CreatedRevision, &item.RetentionExempt, &item.UpdatedRevision, &item.CreatedAt, &item.UpdatedAt, &item.MIMEType, &item.SizeBytes, &item.ETag); err != nil {
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
		SELECT kind,id,remote_item_id,display_name,source_revision,created_revision,event_revision,asset_id,mime_type,size_bytes,etag,retention_exempt,updated_revision,created_at,updated_at,event_at
		FROM (
		  SELECT 0 AS kind,i.id,i.remote_item_id,i.display_name,i.source_revision,i.created_revision,i.updated_revision AS event_revision,
		         COALESCE(i.asset_id,'') AS asset_id,COALESCE(a.detected_mime_type,'') AS mime_type,
		         COALESCE(a.size_bytes,0) AS size_bytes,COALESCE(a.etag,'') AS etag,i.retention_exempt,i.updated_revision,i.created_at,i.updated_at,i.updated_at AS event_at
		  FROM asset_collection_items i LEFT JOIN assets a ON a.id=i.asset_id
		  WHERE i.collection_id=$1 AND i.updated_revision>$2 AND i.updated_revision<=$3
		  UNION ALL
		  SELECT 1 AS kind,i.id,i.remote_item_id,'' AS display_name,'' AS source_revision,0::bigint AS created_revision,i.deleted_revision AS event_revision,
		         '' AS asset_id,'' AS mime_type,0::bigint AS size_bytes,'' AS etag,false AS retention_exempt,0::bigint AS updated_revision,'0001-01-01'::timestamptz AS created_at,'0001-01-01'::timestamptz AS updated_at,i.deleted_at AS event_at
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
		createdRevision, revision                                              int64
		id, remoteItemID, displayName, sourceRevision, assetID, mimeType, etag string
		sizeBytes                                                              int64
		retentionExempt                                                        bool
		updatedRevision                                                        int64
		createdAt, updatedAt, at                                               time.Time
	}
	var events []changeEvent
	for rows.Next() {
		var event changeEvent
		if err := rows.Scan(&event.kind, &event.id, &event.remoteItemID, &event.displayName, &event.sourceRevision, &event.createdRevision, &event.revision, &event.assetID, &event.mimeType, &event.sizeBytes, &event.etag, &event.retentionExempt, &event.updatedRevision, &event.createdAt, &event.updatedAt, &event.at); err != nil {
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
			page.Items = append(page.Items, assets.CollectionItem{ID: event.id, CollectionID: collection.ID, AssetID: event.assetID, RemoteItemID: event.remoteItemID, DisplayName: event.displayName, SourceRevision: event.sourceRevision, CreatedRevision: event.createdRevision, RetentionExempt: event.retentionExempt, UpdatedRevision: event.updatedRevision, MIMEType: event.mimeType, SizeBytes: event.sizeBytes, ETag: event.etag, CreatedAt: event.createdAt, UpdatedAt: event.updatedAt})
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
	err := s.db.QueryRowContext(ctx, `SELECT id,namespace,name,revision,retention_days,created_by_service,created_at,updated_at FROM asset_collections WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&value.ID, &value.Namespace, &value.Name, &value.Revision, &value.RetentionDays, &value.CreatedByService, &value.CreatedAt, &value.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.Collection{}, assets.ErrNotFound
	}
	return value, err
}

func (s *Store) hasCollectionACL(ctx context.Context, id string, subject assets.CollectionSubject) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_collection_acl WHERE collection_id=$1 AND permission='read' AND revoked_at IS NULL AND ((subject_type='user' AND subject_id=$2) OR (subject_type='role' AND subject_id=ANY($3::text[]))))`, id, subject.UserID, subject.RoleIDs).Scan(&allowed)
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

func encodeManagedItemCursor(cursor managedItemCursor) string {
	value, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeManagedItemCursor(value string) (managedItemCursor, bool) {
	if value == "" {
		return managedItemCursor{}, true
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return managedItemCursor{}, false
	}
	var cursor managedItemCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.CreatedAt.IsZero() || cursor.LastID == "" {
		return managedItemCursor{}, false
	}
	return cursor, true
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO asset_derivative_outbox(event_id,asset_id,asset_etag,available_at,created_at)
		SELECT $1,id,etag,$3,$3 FROM assets
		WHERE id=$2 AND upload_status='completed' AND scan_status='clean' AND processing_status='pending'
		  AND detected_mime_type IN ('image/jpeg','image/png','image/webp')
		  AND deleted_at IS NULL AND purged_at IS NULL
		ON CONFLICT(asset_id,asset_etag) DO NOTHING`, result.EventID, result.AssetID, now); err != nil {
		return false, err
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
	for {
		retry, err := s.softDeleteAsset(ctx, assetID, ownerService, now)
		if err != nil || !retry {
			return err
		}
	}
}

func (s *Store) softDeleteAsset(ctx context.Context, assetID, ownerService string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	collectionIDs, err := activeAssetCollectionIDs(ctx, tx, assetID)
	if err != nil {
		return false, err
	}
	revisions := make(map[string]int64, len(collectionIDs))
	if len(collectionIDs) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id,revision FROM asset_collections WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE`, collectionIDs)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id string
			var revision int64
			if err := rows.Scan(&id, &revision); err != nil {
				rows.Close()
				return false, err
			}
			revisions[id] = revision
		}
		if err := finishRows(rows); err != nil {
			return false, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,collection_id FROM asset_collection_items WHERE asset_id=$1 AND deleted_revision IS NULL ORDER BY collection_id,id FOR UPDATE`, assetID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var itemID, collectionID string
		if err := rows.Scan(&itemID, &collectionID); err != nil {
			rows.Close()
			return false, err
		}
		if _, locked := revisions[collectionID]; !locked {
			if err := rows.Close(); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if err := finishRows(rows); err != nil {
		return false, err
	}
	var deletedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT deleted_at FROM assets WHERE id=$1 AND owner_service=$2 AND purged_at IS NULL FOR UPDATE`, assetID, ownerService).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, assets.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if deletedAt.Valid {
		return false, tx.Commit()
	}
	currentCollectionIDs, err := activeAssetCollectionIDs(ctx, tx, assetID)
	if err != nil {
		return false, err
	}
	for _, collectionID := range currentCollectionIDs {
		if _, locked := revisions[collectionID]; !locked {
			return true, nil
		}
	}
	for _, collectionID := range collectionIDs {
		nextRevision := revisions[collectionID] + 1
		result, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET deleted_revision=$3,deleted_at=$4 WHERE collection_id=$1 AND asset_id=$2 AND deleted_revision IS NULL`, collectionID, assetID, nextRevision, now)
		if err != nil {
			return false, err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, collectionID, nextRevision, now); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET deleted_at=$2,updated_at=$2 WHERE id=$1`, assetID, now); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func activeAssetCollectionIDs(ctx context.Context, tx *sql.Tx, assetID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT collection_id FROM asset_collection_items WHERE asset_id=$1 AND deleted_revision IS NULL ORDER BY collection_id`, assetID)
	if err != nil {
		return nil, err
	}
	var collectionIDs []string
	for rows.Next() {
		var collectionID string
		if err := rows.Scan(&collectionID); err != nil {
			rows.Close()
			return nil, err
		}
		collectionIDs = append(collectionIDs, collectionID)
	}
	if err := finishRows(rows); err != nil {
		return nil, err
	}
	return collectionIDs, nil
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
	if err == nil {
		err = s.db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM asset_collection_items i
			JOIN asset_collections c ON c.id=i.collection_id
			WHERE c.deleted_at IS NULL AND c.namespace='line.group.media-sync' AND c.created_by_service='hhc-line-function-bot'
			  AND i.deleted_revision IS NULL AND i.retention_exempt=false
			  AND i.created_at + c.retention_days * interval '1 day' <= $1`, now).Scan(&value.ExpiredCollectionItems)
	}
	if oldestScan.Valid {
		value.OldestScanPending = oldestScan.Time
	}
	if oldestProcessing.Valid {
		value.OldestProcessingPending = oldestProcessing.Time
	}
	return value, err
}

func (s *Store) ListExpiredCollectionItems(ctx context.Context, now time.Time, limit int) ([]retention.Candidate, error) {
	if limit < 1 || limit > retention.BatchSize {
		limit = retention.BatchSize
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.collection_id,i.id
		FROM asset_collection_items i
		JOIN asset_collections c ON c.id=i.collection_id
		WHERE c.deleted_at IS NULL AND c.namespace='line.group.media-sync' AND c.created_by_service='hhc-line-function-bot'
		  AND i.deleted_revision IS NULL AND i.retention_exempt=false
		  AND i.created_at + c.retention_days * interval '1 day' <= $1
		ORDER BY i.created_at,i.id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	var candidates []retention.Candidate
	for rows.Next() {
		var candidate retention.Candidate
		if err := rows.Scan(&candidate.CollectionID, &candidate.ItemID); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, finishRows(rows)
}

func (s *Store) PreviewExpiredCollectionItems(ctx context.Context, now time.Time) ([]retention.Preview, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id,COUNT(*),COALESCE(SUM(a.size_bytes),0)
		FROM asset_collection_items i
		JOIN asset_collections c ON c.id=i.collection_id
		LEFT JOIN assets a ON a.id=i.asset_id
		WHERE c.deleted_at IS NULL AND c.namespace='line.group.media-sync' AND c.created_by_service='hhc-line-function-bot'
		  AND i.deleted_revision IS NULL AND i.retention_exempt=false
		  AND i.created_at + c.retention_days * interval '1 day' <= $1
		GROUP BY c.id ORDER BY c.id`, now)
	if err != nil {
		return nil, err
	}
	var preview []retention.Preview
	for rows.Next() {
		var value retention.Preview
		if err := rows.Scan(&value.CollectionID, &value.CandidateCount, &value.TotalBytes); err != nil {
			rows.Close()
			return nil, err
		}
		preview = append(preview, value)
	}
	return preview, finishRows(rows)
}

func (s *Store) DeleteExpiredCollectionItems(ctx context.Context, collectionID string, itemIDs []string, now time.Time) (retention.DeleteResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return retention.DeleteResult{}, err
	}
	defer tx.Rollback()
	deleted, err := s.deleteCollectionItems(ctx, tx, collectionID, "hhc-line-function-bot", itemIDs, now, true)
	if err != nil {
		return retention.DeleteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return retention.DeleteResult{}, err
	}
	return retention.DeleteResult{Deleted: deleted.result.Deleted, ExemptSkipped: deleted.result.ExemptSkipped, AlreadyRemoved: deleted.result.AlreadyRemoved}, nil
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

func uniqueSortedStrings(values []string) []string {
	result := uniqueStrings(values)
	slices.Sort(result)
	return result
}

type rowSet interface {
	Err() error
	Close() error
}

func finishRows(rows rowSet) error {
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
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

func (s *Store) ClaimProcessing(ctx context.Context, assetID, etag string, now time.Time, lease time.Duration) (assets.Asset, assets.ProcessingClaimState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Asset{}, "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM assets
		WHERE id=$1 AND etag=$2 AND upload_status='completed' AND scan_status='clean'
		  AND processing_status='pending' AND processing_attempts < 5
		  AND detected_mime_type IN ('image/jpeg','image/png','image/webp')
		  AND deleted_at IS NULL AND purged_at IS NULL
		  AND (processing_next_attempt_at IS NULL OR processing_next_attempt_at <= $3)
		  AND (processing_claimed_until IS NULL OR processing_claimed_until < $3)
		FOR UPDATE`, assetID, etag, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return assets.Asset{}, "", err
		}
		asset, getErr := s.GetAsset(ctx, assetID)
		if errors.Is(getErr, assets.ErrNotFound) {
			return assets.Asset{}, assets.ProcessingTerminal, nil
		}
		if getErr != nil {
			return assets.Asset{}, "", getErr
		}
		if asset.ETag == etag && asset.UploadStatus == assets.UploadCompleted && asset.ScanStatus == assets.ScanClean && asset.ProcessingStatus == assets.ProcessingPending && asset.ProcessingAttempts < 5 && (asset.DetectedMIMEType == "image/jpeg" || asset.DetectedMIMEType == "image/png" || asset.DetectedMIMEType == "image/webp") && asset.DeletedAt.IsZero() {
			return asset, assets.ProcessingDeferred, nil
		}
		return asset, assets.ProcessingTerminal, nil
	}
	if err != nil {
		return assets.Asset{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET processing_attempts=processing_attempts+1,processing_claimed_until=$2 WHERE id=$1`, id, now.Add(lease)); err != nil {
		return assets.Asset{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return assets.Asset{}, "", err
	}
	asset, err := s.GetAsset(ctx, id)
	return asset, assets.ProcessingClaimed, err
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
