package postgres

import (
	"context"
	"database/sql"
	"errors"
	"hhc/asset-api/internal/assets"
	"time"
)

// Reconcile rotates the durable retry cursor before storage calls. Reference
// retention and expiry fencing share a row lock; provider I/O runs after commit.
func (s *RecordingCoverStore) Reconcile(ctx context.Context, remove func(context.Context, []string) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recording_covers WHERE cleanup_after<=now() ORDER BY cleanup_after LIMIT 20`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var result error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		keys, cursor, err := s.coverCleanupKeys(ctx, id)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if len(keys) > 0 {
			deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = remove(deleteCtx, keys)
			cancel()
			result = errors.Join(result, err)
			if err == nil {
				// Successful byte deletion needs only the daily late-write sweep;
				// provider failures keep the five-minute retry cursor.
				_, scheduleErr := s.db.ExecContext(ctx, `UPDATE recording_covers SET cleanup_after=now()+interval '1 day' WHERE id=$1 AND cleanup_after=$2`, id, cursor)
				result = errors.Join(result, scheduleErr)
			}
		}
	}
	return result
}
func (s *RecordingCoverStore) coverCleanupKeys(ctx context.Context, id string) ([]string, time.Time, error) {
	var cursor time.Time
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return nil, cursor, err
	}
	defer tx.Rollback()
	// Match owner deletion and policy updates: package before its cover rows.
	var packageID string
	err = tx.QueryRowContext(ctx, `SELECT p.id FROM recording_packages p JOIN recording_covers c ON c.package_id=p.id WHERE c.id=$1 AND c.cleanup_after<=now() FOR UPDATE OF p`, id).Scan(&packageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, cursor, nil
	}
	if err != nil {
		return nil, cursor, err
	}
	c, err := scanCover(tx.QueryRowContext(ctx, `SELECT `+coverColumns+` FROM recording_covers c WHERE id=$1 AND cleanup_after<=now() FOR UPDATE`, id))
	if errors.Is(err, assets.ErrNotFound) {
		return nil, cursor, nil
	}
	if err != nil {
		return nil, cursor, err
	}
	var expired, stale, retained bool
	if err := tx.QueryRowContext(ctx, `SELECT c.expires_at<=now() OR c.state='expired' OR EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id) OR NOT EXISTS(SELECT 1 FROM recording_packages p WHERE p.id=c.package_id AND p.state='ready' AND p.media_expires_at>now()),c.created_at<now()-interval '24 hours',EXISTS(SELECT 1 FROM recording_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL) FROM recording_covers c WHERE id=$1`, id).Scan(&expired, &stale, &retained); err != nil {
		return nil, cursor, err
	}
	expired = expired || (c.Kind == "custom" && stale && !retained)
	// An expiry sweep can remove covers before the package sweep runs. Fence
	// that package too so a subsequent policy extension cannot revive it.
	if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET state='expired',cleanup_after=clock_timestamp() WHERE id=$1 AND state='ready' AND media_expires_at<=clock_timestamp()`, c.PackageID); err != nil {
		return nil, cursor, err
	}
	if err := tx.QueryRowContext(ctx, `UPDATE recording_covers SET cleanup_after=clock_timestamp()+interval '5 minutes',state=CASE WHEN $2 THEN 'expired' ELSE state END WHERE id=$1 RETURNING cleanup_after`, id, expired).Scan(&cursor); err != nil {
		return nil, cursor, err
	}
	var keys []string
	if c.Kind == "custom" && (expired || c.State == "ready" || c.State == "failed" && stale) {
		keys = append(keys, c.InputKey())
	}
	rows, err := tx.QueryContext(ctx, `SELECT claim_id FROM recording_cover_attempts WHERE cover_id=$1 AND ($2 OR (claim_id<>$3 AND claim_id<>$4 AND created_at<now()-interval '6 hours')) ORDER BY created_at`, id, expired, c.OutputAttempt, c.ClaimID)
	if err != nil {
		return nil, cursor, err
	}
	for rows.Next() {
		var attempt string
		if err := rows.Scan(&attempt); err != nil {
			rows.Close()
			return nil, cursor, err
		}
		copy := c
		copy.OutputAttempt = attempt
		count := 1
		if c.Kind == "auto" {
			count = 3
		}
		for i := 1; i <= count; i++ {
			keys = append(keys, copy.Key(i))
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, cursor, err
	}
	return keys, cursor, tx.Commit()
}
