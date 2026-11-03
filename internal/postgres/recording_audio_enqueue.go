package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

const audioEnqueueEligible = `p.owner_service='hhc-web-api' AND p.state='ready' AND p.ready_at IS NOT NULL AND p.media_expires_at>clock_timestamp()
 AND p.final_prefix ~ ('^recordings/packages/'||p.id||'/final/[a-zA-Z0-9-]{1,80}/$')
 AND p.inventory->>'inventoryDigest' ~ '^[0-9a-f]{64}$'
 AND NOT EXISTS(SELECT 1 FROM recording_audio a WHERE a.package_id=p.id)
 AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=p.recording_id)
 AND NOT EXISTS(SELECT 1 FROM recording_broadcast_ranges b WHERE b.capture_id=p.id AND b.revoked)`

// EnqueueRecordingAudio schedules one bounded batch without resetting any state.
// The owner fence precedes package locks, matching deletion and revocation.
func (s *RecordingPackageStore) EnqueueRecordingAudio(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 100 {
		return 0, fmt.Errorf("audio enqueue limit must be 1..100")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Serialize only explicit batch commands; normal recording work is unaffected.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-audio-enqueue',0))`); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.recording_id,p.final_prefix,p.inventory->>'inventoryDigest' FROM recording_packages p WHERE `+audioEnqueueEligible+` ORDER BY p.recording_id,p.id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, recording, prefix, digest string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.recording, &c.prefix, &c.digest); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var count int64
	for _, c := range candidates {
		// A revoked/deleted candidate is simply omitted; never turn it into an error
		// that would prevent other eligible rows from progressing on the next call.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-owner:'||$1,0))`, c.recording); err != nil {
			return 0, err
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT p.id FROM recording_packages p WHERE p.id=$1 AND p.recording_id=$2 AND p.final_prefix=$3 AND p.inventory->>'inventoryDigest'=$4 AND `+audioEnqueueEligible+` FOR UPDATE`, c.id, c.recording, c.prefix, c.digest).Scan(&id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO recording_audio(package_id,version,source_prefix,source_digest) SELECT p.id,'aac-hls-v1',p.final_prefix,p.inventory->>'inventoryDigest' FROM recording_packages p WHERE p.id=$1 AND `+audioEnqueueEligible+` ON CONFLICT DO NOTHING`, id)
		if err != nil {
			return 0, err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		count += added
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
