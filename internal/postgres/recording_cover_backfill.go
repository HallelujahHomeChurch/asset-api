package postgres

import (
	"context"
	"hhc/asset-api/internal/assets"
)

// Backfill returns a stable page and only queues work when explicitly applied.
// Repeating a page is safe; existing auto/custom choices are never changed.
func (s *RecordingCoverStore) Backfill(ctx context.Context, after string, limit int, apply bool) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT p.id FROM recording_packages p WHERE p.id>$1 AND p.state='ready' AND p.media_expires_at>now() AND p.owner_service='hhc-web-api' AND NOT EXISTS(SELECT 1 FROM recording_covers c WHERE c.package_id=p.id AND c.kind='auto') AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=p.recording_id) ORDER BY p.id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if apply {
		for _, id := range ids {
			// Serialize with owner deletion before locking the package.
			var recording string
			if err := tx.QueryRowContext(ctx, "SELECT recording_id FROM recording_packages WHERE id=$1", id).Scan(&recording); err != nil {
				return nil, err
			}
			if err := checkRecordingNotDeleted(ctx, tx, recording); err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,state,expires_at) SELECT $1,id,recording_id,actor_id,'auto','auto','pending',media_expires_at FROM recording_packages WHERE id=$2 AND state='ready' AND media_expires_at>now() ON CONFLICT DO NOTHING`, newStoreID(), id)
			if err != nil {
				return nil, err
			}
		}
	}
	return ids, tx.Commit()
}
