package postgres

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strconv"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/lifecycle"
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
	row := s.db.QueryRowContext(ctx, `SELECT id,namespace,owner_service,owner_type,owner_id,purpose,locale,original_file_name,object_key,expected_mime_type,detected_mime_type,size_bytes,checksum_sha256,etag,upload_status,scan_status,scan_details,processing_status,visibility,created_at,updated_at,COALESCE(deleted_at,'0001-01-01'::timestamptz),scan_attempts,processing_attempts FROM assets WHERE id=$1 AND purged_at IS NULL`, id)
	var value assets.Asset
	err := row.Scan(&value.ID, &value.Namespace, &value.OwnerService, &value.OwnerType, &value.OwnerID, &value.Purpose, &value.Locale, &value.OriginalFileName, &value.ObjectKey, &value.ExpectedMIMEType, &value.DetectedMIMEType, &value.SizeBytes, &value.ChecksumSHA256, &value.ETag, &value.UploadStatus, &value.ScanStatus, &value.ScanDetails, &value.ProcessingStatus, &value.Visibility, &value.CreatedAt, &value.UpdatedAt, &value.DeletedAt, &value.ScanAttempts, &value.ProcessingAttempts)
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

func (s *Store) CompleteUpload(ctx context.Context, asset assets.Asset, session assets.UploadSession) error {
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
		return nil
	}
	if assetStatus != string(assets.UploadCreated) || sessionStatus != string(assets.UploadCreated) {
		return assets.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE assets SET detected_mime_type=$2,size_bytes=$3,checksum_sha256=$4,etag=$5,upload_status=$6,scan_status=$7,updated_at=$8 WHERE id=$1`, asset.ID, asset.DetectedMIMEType, asset.SizeBytes, asset.ChecksumSHA256, asset.ETag, asset.UploadStatus, asset.ScanStatus, asset.UpdatedAt)
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
	return tx.Commit()
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
	updated, err := tx.ExecContext(ctx, `UPDATE assets SET scan_status=$2,scan_details=$3,scan_claimed_until=NULL,updated_at=$5 WHERE id=$1 AND scan_status='pending' AND etag=$4 AND deleted_at IS NULL AND purged_at IS NULL`, result.AssetID, result.Status, result.Details, result.ETag, now)
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

func (s *Store) ScheduleScanRetry(ctx context.Context, assetID, details string, nextAttempt, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET scan_details=$2,scan_next_attempt_at=$3,scan_claimed_until=NULL,updated_at=$4 WHERE id=$1 AND scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL`, assetID, details, nextAttempt, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) SoftDeleteAsset(ctx context.Context, assetID, ownerService string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET deleted_at=COALESCE(deleted_at,$3),updated_at=$3 WHERE id=$1 AND owner_service=$2 AND purged_at IS NULL`, assetID, ownerService, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) RequeueFailedScan(ctx context.Context, assetID, ownerService string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE assets SET scan_status='pending',scan_details='',scan_attempts=0,scan_next_attempt_at=$3,scan_claimed_until=NULL,updated_at=$3 WHERE id=$1 AND owner_service=$2 AND scan_status='failed' AND deleted_at IS NULL AND purged_at IS NULL`, assetID, ownerService, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return assets.ErrInvalidInput
	}
	return nil
}

func (s *Store) GetOperations(ctx context.Context, now time.Time) (assets.Operations, error) {
	var value assets.Operations
	var oldest sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL),
		  COUNT(*) FILTER (WHERE scan_status='failed' AND deleted_at IS NULL AND purged_at IS NULL),
		  MIN(updated_at) FILTER (WHERE scan_status='pending' AND deleted_at IS NULL AND purged_at IS NULL),
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
		FROM assets`, now).Scan(&value.ScanPending, &value.ScanFailed, &oldest, &value.PurgePending)
	if oldest.Valid {
		value.OldestScanPending = oldest.Time
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
