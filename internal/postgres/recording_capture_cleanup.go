package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
)

// Unsealed capture namespaces have no package row. Sweep declared keys after
// terminal/expiry and repeat daily to catch PUTs using earlier signed URLs.
func (s *RecordingCaptureStore) ReconcileCaptures(ctx context.Context, deleteObjects func(context.Context, []string) error) error {
	// Fence unfinished sealed jobs at the original capture deadline before the
	// ordinary package cleanup pass. Normal ready VOD remains independent.
	expired, err := s.db.QueryContext(ctx, `SELECT c.id FROM recording_captures c LEFT JOIN recording_packages p ON p.id=c.package_id WHERE c.expires_at<=clock_timestamp() AND c.state IN ('freezing','validating') AND p.state IN ('freezing','validating') ORDER BY c.expires_at LIMIT 10`)
	if err != nil {
		return err
	}
	expiredIDs := []string{}
	for expired.Next() {
		var id string
		if err = expired.Scan(&id); err != nil {
			expired.Close()
			return err
		}
		expiredIDs = append(expiredIDs, id)
	}
	err = expired.Err()
	expired.Close()
	if err != nil {
		return err
	}
	for _, id := range expiredIDs {
		if _, err = s.GetCapture(ctx, id); err != nil {
			return err
		}
	}
	rows, err := s.db.QueryContext(ctx, `UPDATE recording_captures SET cleanup_after=clock_timestamp()+interval '5 minutes' WHERE id IN (SELECT id FROM recording_captures WHERE package_id IS NULL AND cleanup_after<=clock_timestamp() AND GREATEST(expires_at+interval '6 hours',COALESCE(terminal_at,expires_at)+interval '7 days')<=clock_timestamp() AND (state IN ('failed','expired','aborted') OR expires_at<=clock_timestamp()) ORDER BY cleanup_after LIMIT 10 FOR UPDATE SKIP LOCKED) RETURNING id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
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
	var failures error
	for _, id := range ids {
		_, err := s.UpdateCapture(ctx, id, func(c *assets.RecordingCapture) error {
			keys := []string{}
			for _, o := range c.Objects {
				keys = append(keys, (assets.RecordingPackage{ID: c.ID}).StagingKey(o.Path))
				if len(keys) == 1000 {
					if err := deleteObjects(ctx, keys); err != nil {
						return err
					}
					keys = nil
				}
			}
			if len(keys) > 0 {
				return deleteObjects(ctx, keys)
			}
			return nil
		})
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE recording_captures SET cleanup_after=clock_timestamp()+interval '24 hours' WHERE id=$1`, id); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}
