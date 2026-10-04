package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

// Previews use the same two slots and lock order as source/validation jobs.
func (s *RecordingPackageStore) ClaimPackagePreview(ctx context.Context) (RecordingPackageClaim, error) {
	var c RecordingPackageClaim
	if _, err := s.db.ExecContext(ctx, `UPDATE recording_packages SET preview_state='failed' WHERE preview_state='processing' AND preview_attempts=3 AND preview_claimed_until<=clock_timestamp()`); err != nil {
		return c, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE leased_until IS NULL OR leased_until<=clock_timestamp() ORDER BY slot FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM recording_packages WHERE state='ready' AND media_expires_at>clock_timestamp() AND preview_state IN ('pending','processing') AND preview_attempts<3 AND preview_next_attempt_at<=clock_timestamp() AND (preview_claimed_until IS NULL OR preview_claimed_until<=clock_timestamp()) AND NOT EXISTS(SELECT 1 FROM recording_packages WHERE state IN ('freezing','validating') AND validation_attempts<3 AND next_attempt_at<=clock_timestamp()) AND NOT EXISTS(SELECT 1 FROM recording_sources WHERE state IN ('finalizing','queued','processing') AND processing_attempts<3 AND retry_until>clock_timestamp()) ORDER BY ready_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.ClaimID = newStoreID()
	if _, err = tx.ExecContext(ctx, `UPDATE recording_packages SET preview_state='processing',preview_claim_id=$2,preview_claimed_until=clock_timestamp()+interval '2 minutes',preview_attempts=preview_attempts+1 WHERE id=$1`, id, c.ClaimID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=clock_timestamp()+interval '2 minutes' WHERE slot=$1`, slot, id, c.ClaimID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_preview_attempts(claim_id,package_id) VALUES($1,$2)`, c.ClaimID, id); err != nil {
		return c, err
	}
	c.Package, err = scanRecordingPackage(tx.QueryRowContext(ctx, `SELECT `+recordingPackageSelectColumns+` FROM recording_packages WHERE id=$1`, id))
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (s *RecordingPackageStore) previewTransaction(ctx context.Context, id, claim string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>clock_timestamp() FOR UPDATE`, id, claim).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT id FROM recording_packages WHERE id=$1 AND state='ready' AND media_expires_at>clock_timestamp() AND preview_state='processing' AND preview_claim_id=$2 AND preview_claimed_until>clock_timestamp() FOR UPDATE`, id, claim).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *RecordingPackageStore) HeartbeatPackagePreview(ctx context.Context, id, claim string) error {
	return s.previewTransaction(ctx, id, claim, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()+interval '2 minutes' WHERE claim_id=$1`, claim); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_packages SET preview_claimed_until=clock_timestamp()+interval '2 minutes' WHERE id=$1`, id)
		return err
	})
}

// The short publication holds slot then package locks. The object pointer is
// create-only, so even an ambiguous provider response cannot replace a winner.
func (s *RecordingPackageStore) FinishPackagePreview(ctx context.Context, id, claim string, ready bool, publish func(context.Context) error) error {
	if ready && publish == nil {
		return assets.ErrInvalidInput
	}
	return s.previewTransaction(ctx, id, claim, func(tx *sql.Tx) error {
		if ready {
			pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := publish(pubCtx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET preview_state=CASE WHEN $2 THEN 'ready' WHEN preview_attempts>=3 THEN 'failed' ELSE 'pending' END,preview_claim_id=NULL,preview_claimed_until=NULL,preview_next_attempt_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, id, ready); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE claim_id=$1`, claim)
		return err
	})
}
