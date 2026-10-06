package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

type RecordingSourceStore struct {
	db    *sql.DB
	locks *recordingSessionStore
}

func NewRecordingSourceStore(db *sql.DB) *RecordingSourceStore {
	return &RecordingSourceStore{db: db, locks: newRecordingSessionStore(db)}
}

func (s *RecordingSourceStore) WithSessionLock(ctx context.Context, id string, fn func(context.Context) error) error {
	return s.locks.WithSessionLock(ctx, "source-"+id, fn)
}

const recordingSourceColumns = `id,owner_service,actor_id,recording_id,idempotency_key,file_name,size_bytes,checksum_sha256,block_count,state,created_at,expires_at,completed_at,retry_until,COALESCE(staging_etag,''),COALESCE(copy_attempt_id,''),COALESCE(source_key,''),COALESCE(source_etag,''),source_verified_at,processing_attempts,COALESCE(processing_error,''),COALESCE(package_id,''),processing_progress`

func scanRecordingSource(row *sql.Row) (assets.RecordingSource, error) {
	var p assets.RecordingSource
	var progress []byte
	err := row.Scan(&p.ID, &p.OwnerService, &p.ActorID, &p.RecordingID, &p.IdempotencyKey, &p.FileName, &p.SizeBytes, &p.ChecksumSHA256, &p.BlockCount, &p.State, &p.CreatedAt, &p.ExpiresAt, &p.CompletedAt, &p.RetryUntil, &p.StagingETag, &p.CopyAttemptID, &p.SourceKey, &p.SourceETag, &p.SourceVerifiedAt, &p.ProcessingAttempts, &p.FailureCode, &p.PackageID, &progress)
	if errors.Is(err, sql.ErrNoRows) {
		return p, assets.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.ProcessingProgress, err = decodeRecordingProgress(progress)
	return p, err
}

func (s *RecordingSourceStore) Get(ctx context.Context, id string) (assets.RecordingSource, error) {
	return scanRecordingSource(s.locks.statements(ctx).QueryRowContext(ctx, `SELECT `+recordingSourceColumns+` FROM recording_sources WHERE id=$1`, id))
}
func (s *RecordingSourceStore) FindByIdempotency(ctx context.Context, key string) (assets.RecordingSource, error) {
	return scanRecordingSource(s.locks.statements(ctx).QueryRowContext(ctx, `SELECT `+recordingSourceColumns+` FROM recording_sources WHERE idempotency_key=$1`, key))
}

func (s *RecordingSourceStore) Create(ctx context.Context, p assets.RecordingSource) error {
	if p.State != "uploading" || p.CompletedAt != nil || p.RetryUntil != nil || p.StagingETag != "" {
		return assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRecordingNotDeleted(ctx, tx, p.RecordingID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-source-actor:' || $1,0))`, p.ActorID); err != nil {
		return err
	}
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_sources WHERE actor_id=$1 AND (state IN ('finalizing','queued','processing') OR (state='uploading' AND expires_at > clock_timestamp())))`, p.ActorID).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return assets.ErrConflict
	}
	// Expired uploads may be replaced; accepted processing inputs are retained
	// until their worker/lifecycle releases them, never by creating a new source.
	if _, err := tx.ExecContext(ctx, `UPDATE recording_sources SET state='expired' WHERE actor_id=$1 AND state='uploading' AND expires_at<=clock_timestamp()`, p.ActorID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_sources(id,owner_service,actor_id,recording_id,idempotency_key,file_name,size_bytes,checksum_sha256,block_count,state,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, p.ID, p.OwnerService, p.ActorID, p.RecordingID, p.IdempotencyKey, p.FileName, p.SizeBytes, p.ChecksumSHA256, p.BlockCount, p.State, p.CreatedAt, p.ExpiresAt)
	if err != nil {
		return mapCollectionError(err)
	}
	return tx.Commit()
}

// The row itself is the durable finalize work. A retry can discover it without
// a second queue send; provider bytes remain untrusted until the worker verifies.
func (s *RecordingSourceStore) Finalize(ctx context.Context, id, etag string, at time.Time) error {
	if etag == "" {
		return assets.ErrInvalidInput
	}
	result, err := s.locks.statements(ctx).ExecContext(ctx, `UPDATE recording_sources SET state='finalizing',staging_etag=$2,completed_at=$3,retry_until=$3+interval '7 days' WHERE id=$1 AND state='uploading' AND expires_at>$3`, id, etag, at)
	return recordingTransitionResult(result, err)
}

var _ assets.RecordingSourceRepository = (*RecordingSourceStore)(nil)
