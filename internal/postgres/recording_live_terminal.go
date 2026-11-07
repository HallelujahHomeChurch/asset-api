package postgres

import "context"

// FinishTerminalLiveClaim runs only after the worker has joined its provider work.
// Terminal authority permits cleanup, never another publication or retry.
func (s *RecordingCaptureStore) FinishTerminalLiveClaim(ctx context.Context, claim RecordingLiveClaim) (bool, error) {
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Keep owner -> capture -> live -> slot order, including deleted recordings.
	if _, err = recordingDeleted(ctx, tx, claim.Capture.RecordingID); err != nil {
		return false, err
	}
	var terminal bool
	var packageID *string
	err = tx.QueryRowContext(ctx, `SELECT terminal_at IS NOT NULL OR state IN ('aborted','expired','failed') OR expires_at<=clock_timestamp(),package_id FROM recording_captures WHERE id=$1 AND recording_id=$2 FOR UPDATE`, claim.Capture.ID, claim.Capture.RecordingID).Scan(&terminal, &packageID)
	if err != nil {
		return false, err
	}
	if !terminal && packageID != nil {
		if err = tx.QueryRowContext(ctx, `SELECT state IN ('failed','expired') FROM recording_packages WHERE id=$1`, *packageID).Scan(&terminal); err != nil {
			return false, err
		}
	}
	if !terminal {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_live SET claim_id=NULL,claimed_until=NULL WHERE capture_id=$1 AND claim_id=$2`, claim.Capture.ID, claim.ClaimID); err != nil {
		return false, err
	}
	if err = releaseLiveClaim(ctx, tx, claim); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
