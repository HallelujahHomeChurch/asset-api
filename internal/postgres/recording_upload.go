package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

type RecordingUploadStore struct{ db *sql.DB }

type recordingLockConnKey struct{}

type recordingStatements interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *RecordingUploadStore) statements(ctx context.Context) recordingStatements {
	if conn, ok := ctx.Value(recordingLockConnKey{}).(*sql.Conn); ok {
		return conn
	}
	return s.db
}

type RecordingValidationClaim struct {
	Session  assets.RecordingUploadSession
	ClaimID  string
	Attempts int
}

func NewRecordingUploadStore(db *sql.DB) *RecordingUploadStore { return &RecordingUploadStore{db: db} }

// Serialize R2 completion and deletion across API/worker processes. A failed
// contender retries instead of holding every pool connection waiting for a lock.
func (s *RecordingUploadStore) WithSessionLock(ctx context.Context, id string, run func(context.Context) error) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended('recording-session:' || $1,0))`, id).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return assets.ErrConflict
	}
	defer func() {
		unlockCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var released bool
		unlockErr := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended('recording-session:' || $1,0))`, id).Scan(&released)
		if unlockErr != nil || !released {
			if unlockErr == nil {
				unlockErr = errors.New("recording session lock was not held")
			}
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			resultErr = errors.Join(resultErr, unlockErr)
		}
	}()
	return run(context.WithValue(ctx, recordingLockConnKey{}, conn))
}

const recordingUploadColumns = `id,asset_version_id,owner_service,admin_id,recording_id,file_name,object_key,upload_id,idempotency_key,status,size_bytes,checksum_sha256,created_at,expires_at`
const selectRecordingUploadColumns = recordingUploadColumns + `,COALESCE(duration_seconds,0),uploaded_at`

func scanRecordingUpload(row *sql.Row) (assets.RecordingUploadSession, error) {
	var s assets.RecordingUploadSession
	err := row.Scan(&s.ID, &s.AssetVersionID, &s.OwnerService, &s.AdminID, &s.RecordingID, &s.FileName, &s.ObjectKey, &s.UploadID, &s.IdempotencyKey, &s.Status, &s.SizeBytes, &s.ChecksumSHA256, &s.CreatedAt, &s.ExpiresAt, &s.DurationSeconds, &s.UploadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, assets.ErrNotFound
	}
	return s, err
}

func (s *RecordingUploadStore) FindByIdempotency(ctx context.Context, key string) (assets.RecordingUploadSession, error) {
	return scanRecordingUpload(s.statements(ctx).QueryRowContext(ctx, `SELECT `+selectRecordingUploadColumns+` FROM recording_uploads WHERE idempotency_key=$1`, key))
}

func (s *RecordingUploadStore) Get(ctx context.Context, id string) (assets.RecordingUploadSession, error) {
	return scanRecordingUpload(s.statements(ctx).QueryRowContext(ctx, `SELECT `+selectRecordingUploadColumns+` FROM recording_uploads WHERE id=$1`, id))
}

func (s *RecordingUploadStore) GetByVersion(ctx context.Context, versionID string) (assets.RecordingUploadSession, error) {
	return scanRecordingUpload(s.statements(ctx).QueryRowContext(ctx, `SELECT `+selectRecordingUploadColumns+` FROM recording_uploads WHERE asset_version_id=$1`, versionID))
}

func (s *RecordingUploadStore) Create(ctx context.Context, upload assets.RecordingUploadSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Fixed lock order serializes both caps across concurrent requests and processes.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-admin:' || $1,0))`, upload.AdminID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-item:' || $1,0))`, upload.RecordingID); err != nil {
		return err
	}
	var active, undeleted int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recording_uploads WHERE admin_id=$1 AND status IN ('created','completing','validating') AND expires_at > now()`, upload.AdminID).Scan(&active); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recording_uploads WHERE recording_id=$1`, upload.RecordingID).Scan(&undeleted); err != nil {
		return err
	}
	if active >= 2 || undeleted >= 1 {
		return assets.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_uploads (`+recordingUploadColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		upload.ID, upload.AssetVersionID, upload.OwnerService, upload.AdminID, upload.RecordingID, upload.FileName, upload.ObjectKey, upload.UploadID, upload.IdempotencyKey, upload.Status, upload.SizeBytes, upload.ChecksumSHA256, upload.CreatedAt, upload.ExpiresAt)
	if err != nil {
		return mapCollectionError(err)
	}
	return tx.Commit()
}

func (s *RecordingUploadStore) ClaimCompletion(ctx context.Context, id string) error {
	return s.transition(ctx, id, "created", "completing")
}

func (s *RecordingUploadStore) MarkValidating(ctx context.Context, id string) error {
	result, err := s.statements(ctx).ExecContext(ctx, `UPDATE recording_uploads SET status='validating',uploaded_at=COALESCE(uploaded_at,clock_timestamp()) WHERE id=$1 AND status='completing'`, id)
	return recordingTransitionResult(result, err)
}

func (s *RecordingUploadStore) MarkCancelled(ctx context.Context, id string) error {
	return s.transition(ctx, id, "created", "cancelled")
}

func (s *RecordingUploadStore) MarkDeleted(ctx context.Context, id string) error {
	result, err := s.statements(ctx).ExecContext(ctx, `UPDATE recording_uploads SET status='deleted' WHERE id=$1 AND status='deleting'`, id)
	return recordingTransitionResult(result, err)
}

func (s *RecordingUploadStore) ClaimDeletion(ctx context.Context, id string) error {
	result, err := s.statements(ctx).ExecContext(ctx, `UPDATE recording_uploads SET status='deleting' WHERE id=$1 AND status<>'deleted'`, id)
	return recordingTransitionResult(result, err)
}

func (s *RecordingUploadStore) ExpiredAbandoned(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT asset_version_id FROM recording_uploads WHERE (expires_at<$1 AND uploaded_at IS NULL AND status IN ('created','completing','failed','cancelled')) OR status='deleting' ORDER BY expires_at LIMIT 100`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		versions = append(versions, id)
	}
	return versions, rows.Err()
}

func (s *RecordingUploadStore) ClaimAbandonedDeletion(ctx context.Context, id string, now time.Time) error {
	result, err := s.statements(ctx).ExecContext(ctx, `UPDATE recording_uploads SET status='deleting' WHERE id=$1 AND (status='deleting' OR (expires_at <= $2 AND uploaded_at IS NULL AND status IN ('created','completing','failed','cancelled')))`, id, now)
	return recordingTransitionResult(result, err)
}

func (s *RecordingUploadStore) transition(ctx context.Context, id, from, to string) error {
	result, err := s.statements(ctx).ExecContext(ctx, `UPDATE recording_uploads SET status=$3 WHERE id=$1 AND status=$2`, id, from, to)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return assets.ErrConflict
	}
	return nil
}

func (s *RecordingUploadStore) ClaimValidation(ctx context.Context, now time.Time, lease time.Duration) (RecordingValidationClaim, error) {
	if lease <= 0 {
		return RecordingValidationClaim{}, assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordingValidationClaim{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE recording_uploads SET status='failed',validation_claim_id=NULL,validation_claimed_until=NULL,validation_error='validation attempts exhausted' WHERE status='validating' AND validation_attempts >= 3 AND validation_claimed_until < $1`, now); err != nil {
		return RecordingValidationClaim{}, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM recording_uploads WHERE status='validating' AND (validation_claimed_until IS NULL OR validation_claimed_until < $1) AND validation_attempts < 3 ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return RecordingValidationClaim{}, err
		}
		return RecordingValidationClaim{}, assets.ErrNotFound
	}
	if err != nil {
		return RecordingValidationClaim{}, err
	}
	claimID := newStoreID()
	var attempts int
	err = tx.QueryRowContext(ctx, `UPDATE recording_uploads SET validation_claim_id=$2, validation_claimed_until=$3, validation_attempts=validation_attempts+1 WHERE id=$1 RETURNING validation_attempts`, id, claimID, now.Add(lease)).Scan(&attempts)
	if err != nil {
		return RecordingValidationClaim{}, err
	}
	if err = tx.Commit(); err != nil {
		return RecordingValidationClaim{}, err
	}
	session, err := s.Get(ctx, id)
	return RecordingValidationClaim{Session: session, ClaimID: claimID, Attempts: attempts}, err
}

func (s *RecordingUploadStore) MarkRecordingReady(ctx context.Context, id, claimID string, duration float64) error {
	if duration <= 0 {
		return assets.ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recording_uploads SET status='ready',duration_seconds=$3,validation_claim_id=NULL,validation_claimed_until=NULL,validation_error=NULL WHERE id=$1 AND validation_claim_id=$2 AND status='validating' AND validation_claimed_until > now()`, id, claimID, duration)
	return recordingTransitionResult(result, err)
}

func (s *RecordingUploadStore) FinishRecordingValidation(ctx context.Context, id, claimID string, permanent bool, message string, now time.Time) error {
	if len(message) > 200 {
		message = message[:200]
	}
	status := "validating"
	if permanent {
		status = "failed"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recording_uploads SET status=$3,validation_claim_id=NULL,validation_claimed_until=$4,validation_error=$5 WHERE id=$1 AND validation_claim_id=$2 AND status='validating'`, id, claimID, status, now.Add(5*time.Minute), message)
	return recordingTransitionResult(result, err)
}

func recordingTransitionResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return assets.ErrConflict
	}
	return nil
}
