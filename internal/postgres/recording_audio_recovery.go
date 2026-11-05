package postgres

import (
	"context"
	"database/sql"
	"errors"
	"hhc/asset-api/internal/assets"
)

type RecordingAudioRetryPlan struct {
	PackageID       string  `json:"packageId"`
	LastClaim       string  `json:"lastClaim"`
	SourceDigest    string  `json:"sourceDigest"`
	InputBytes      int64   `json:"inputBytes"`
	Segments        int64   `json:"segments"`
	DurationSeconds float64 `json:"durationSeconds"`
}

const audioRetryEligible = audioAlive + ` AND a.state='failed' AND a.attempts=3 AND a.claim_id IS NULL AND a.claimed_until IS NULL`
const audioLatestClaim = `(SELECT claim_id FROM recording_audio_attempts WHERE package_id=p.id ORDER BY created_at DESC,claim_id DESC LIMIT 1)`

// A preview grants no work; apply must still match this exact failed attempt.
func (s *RecordingPackageStore) PlanRecordingAudioRetry(ctx context.Context, id string) (RecordingAudioRetryPlan, error) {
	var p RecordingAudioRetryPlan
	err := s.db.QueryRowContext(ctx, `SELECT p.id,`+audioLatestClaim+`,a.source_digest,x.bytes,(r->>'segmentCount')::bigint,(r->>'durationSeconds')::double precision FROM recording_packages p JOIN recording_audio a ON a.package_id=p.id CROSS JOIN LATERAL (SELECT r FROM jsonb_array_elements(p.inventory->'renditions') r ORDER BY (r->>'videoBitrate')::bigint LIMIT 1) low CROSS JOIN LATERAL (SELECT sum((o->>'sizeBytes')::bigint) bytes FROM jsonb_array_elements(p.inventory->'objects') o WHERE o->>'path' LIKE (r->>'name')||'/%' AND o->>'path' NOT LIKE '%.m3u8') x WHERE p.id=$1 AND `+audioRetryEligible, id).Scan(&p.PackageID, &p.LastClaim, &p.SourceDigest, &p.InputBytes, &p.Segments, &p.DurationSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		err = assets.ErrConflict
	}
	return p, err
}

func (s *RecordingPackageStore) RetryRecordingAudio(ctx context.Context, id, lastClaim string) error {
	if id == "" || lastClaim == "" {
		return assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recordingID string
	err = tx.QueryRowContext(ctx, `SELECT recording_id FROM recording_packages WHERE id=$1`, id).Scan(&recordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	if err = checkRecordingNotDeleted(ctx, tx, recordingID); err != nil {
		return err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT p.id FROM recording_packages p JOIN recording_audio a ON a.package_id=p.id WHERE p.id=$1 FOR UPDATE OF p,a`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	// Read the attempt fence after any concurrent claim/finish has released its locks.
	err = tx.QueryRowContext(ctx, `SELECT p.id FROM recording_packages p JOIN recording_audio a ON a.package_id=p.id WHERE p.id=$1 AND `+audioRetryEligible+` AND `+audioLatestClaim+`=$2`, id, lastClaim).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	// Leave only one attempt available; retain all immutable attempt records.
	if _, err = tx.ExecContext(ctx, `UPDATE recording_audio SET state='pending',attempts=2,next_attempt_at=clock_timestamp() WHERE package_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
