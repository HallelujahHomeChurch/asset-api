package postgres

import (
	"context"
	"database/sql"

	"hhc/asset-api/internal/assets"
)

type RecordingDeletionStore struct{ db *sql.DB }

func NewRecordingDeletionStore(db *sql.DB) *RecordingDeletionStore {
	return &RecordingDeletionStore{db: db}
}

// The owning CMS has already checked publication permissions. The tombstone
// also fences a create request which crossed the CMS boundary before deletion.
func (s *RecordingDeletionStore) DeleteRecording(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-owner:' || $1,0))`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_deletions(recording_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_captures SET state='expired',terminal_at=COALESCE(terminal_at,clock_timestamp()),terminal_reason=COALESCE(terminal_reason,'recording_deleted'),cleanup_after=clock_timestamp() WHERE recording_id=$1`, id); err != nil {
		return err
	}
	// Workers acquire slots before source/package rows. Keep this lock order;
	// state changes make every subsequent heartbeat/checkpoint fail closed.
	if _, err = tx.ExecContext(ctx, `SELECT slot FROM recording_processing_slots ORDER BY slot FOR UPDATE`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_sources SET state='expired',claim_id=NULL,claimed_until=NULL,cleanup_after=clock_timestamp() WHERE recording_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_packages SET state='expired',claim_id=NULL,claimed_until=NULL,preview_claim_id=NULL,preview_claimed_until=NULL,media_expires_at=LEAST(media_expires_at,clock_timestamp()),cleanup_after=clock_timestamp() WHERE recording_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_package_attempts SET state='abandoned',finished_at=clock_timestamp() WHERE state='processing' AND package_id IN(SELECT id FROM recording_packages WHERE recording_id=$1)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_source_attempts SET state='abandoned',finished_at=clock_timestamp() WHERE state='processing' AND source_id IN(SELECT id FROM recording_sources WHERE recording_id=$1)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_covers SET state='expired',claim_id=NULL,claimed_until=NULL,cleanup_after=now() WHERE recording_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE job_id IN(SELECT id FROM recording_packages WHERE recording_id=$1 UNION SELECT id FROM recording_sources WHERE recording_id=$1 UNION SELECT id FROM recording_covers WHERE recording_id=$1)`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func checkRecordingNotDeleted(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-owner:' || $1,0))`, id); err != nil {
		return err
	}
	var deleted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_deletions WHERE recording_id=$1)`, id).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return assets.ErrConflict
	}
	return nil
}
