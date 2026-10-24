package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/diagnostics"
)

// Active grants only extend read_grant_until. Producer journals and objects stay
// with their mutation owner; terminal/package/recovery work keeps the full path.
func (s *RecordingCaptureStore) withActiveCaptureGrant(ctx context.Context, id string, authority []byte, fn func(*assets.RecordingCapture, assets.RecordingBroadcastReader) error) (assets.RecordingCapture, bool, error) {
	var c assets.RecordingCapture
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	diagnostics.Default.Observe(diagnostics.CaptureBegin, started, err)
	if err != nil {
		return c, true, err
	}
	defer tx.Rollback()
	var recording string
	if err = tx.QueryRowContext(ctx, `SELECT recording_id FROM recording_captures WHERE id=$1`, id).Scan(&recording); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = assets.ErrNotFound
		}
		return c, true, err
	}
	started = time.Now()
	_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-owner:' || $1,0))`, recording)
	diagnostics.Default.Observe(diagnostics.CaptureOwnerLock, started, err)
	if err != nil {
		return c, true, err
	}
	started = time.Now()
	c, err = scanCapture(tx.QueryRowContext(ctx, `SELECT `+captureObservationColumns+` FROM recording_captures WHERE id=$1 FOR UPDATE`, id))
	diagnostics.Default.Observe(diagnostics.CaptureRow, started, err)
	if err != nil {
		return c, true, err
	}
	if c.State != "uploading" || c.PackageID != nil || c.Inventory != nil || c.TerminalAt != nil || c.Recovery.Active || !time.Now().Before(c.ExpiresAt) {
		return c, false, nil
	}
	var deleted bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_deletions WHERE recording_id=$1)`, recording).Scan(&deleted); err != nil {
		return c, true, err
	}
	if deleted {
		return c, false, nil
	}
	started = time.Now()
	err = tx.QueryRowContext(ctx, `SELECT published_revision,published_sequence,published_media_end,last_advanced_at,ended_at,ended AND published_revision=revision,read_grant_until FROM recording_live WHERE capture_id=$1 FOR UPDATE`, id).Scan(&c.Progress.Revision, &c.Progress.LastSequence, &c.Progress.MediaEndSeconds, &c.Progress.LastAdvancedAt, &c.Progress.EndedAt, &c.Progress.Ended, &c.ReadGrantUntil)
	diagnostics.Default.Observe(diagnostics.CaptureLiveRow, started, err)
	if err != nil {
		return c, true, err
	}
	var prior time.Time
	if c.ReadGrantUntil != nil {
		prior = *c.ReadGrantUntil
	}
	started = time.Now()
	err = fn(&c, broadcastReader{query: tx, broadcastObjects: s.broadcastObjects, authority: authority})
	diagnostics.Default.Observe(diagnostics.CaptureCallback, started, err)
	if err != nil {
		return c, true, err
	}
	if c.ReadGrantUntil != nil && (prior.IsZero() || c.ReadGrantUntil.After(prior)) {
		if _, err = tx.ExecContext(ctx, `UPDATE recording_live SET read_grant_until=GREATEST(read_grant_until,$2) WHERE capture_id=$1`, id, c.ReadGrantUntil); err != nil {
			return c, true, err
		}
	}
	started = time.Now()
	err = tx.Commit()
	diagnostics.Default.Observe(diagnostics.CaptureCommit, started, err)
	return c, true, err
}
