package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

const audioAlive = `p.state='ready' AND p.media_expires_at>clock_timestamp() AND p.final_prefix=a.source_prefix AND p.inventory->>'inventoryDigest'=a.source_digest AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=p.recording_id) AND NOT EXISTS(SELECT 1 FROM recording_broadcast_ranges b WHERE b.capture_id=p.id AND b.revoked)`
const audioWakeCandidate = `SELECT a.package_id FROM recording_audio a JOIN recording_packages p ON p.id=a.package_id WHERE ` + audioAlive + ` AND a.state IN ('pending','processing') AND a.attempts<3 AND a.next_attempt_at<=clock_timestamp() AND (a.claimed_until IS NULL OR a.claimed_until<=clock_timestamp()) AND ` + previewWorkWaiting + ` ORDER BY a.created_at`

// Called only by new ready transitions; migration never schedules old packages.
func enqueueRecordingAudio(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO recording_audio(package_id,version,source_prefix,source_digest) SELECT id,'aac-hls-v1',final_prefix,inventory->>'inventoryDigest' FROM recording_packages WHERE id=$1 AND state='ready' AND media_expires_at>clock_timestamp() ON CONFLICT DO NOTHING`, id)
	return err
}

func (s *RecordingPackageStore) ClaimPackageAudio(ctx context.Context) (RecordingPackageClaim, error) {
	var c RecordingPackageClaim
	if _, err := s.db.ExecContext(ctx, `UPDATE recording_audio SET state='failed' WHERE state='processing' AND attempts=3 AND claimed_until<=clock_timestamp()`); err != nil {
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
	err = tx.QueryRowContext(ctx, audioWakeCandidate+` FOR UPDATE OF p,a SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.ClaimID = newStoreID()
	if _, err = tx.ExecContext(ctx, `UPDATE recording_audio SET state='processing',claim_id=$2,claimed_until=clock_timestamp()+interval '2 minutes',attempts=attempts+1 WHERE package_id=$1`, id, c.ClaimID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=clock_timestamp()+interval '2 minutes' WHERE slot=$1`, slot, id, c.ClaimID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_audio_attempts(claim_id,package_id) VALUES($1,$2)`, c.ClaimID, id); err != nil {
		return c, err
	}
	c.Package, err = scanRecordingPackage(tx.QueryRowContext(ctx, `SELECT `+recordingPackageSelectColumns+` FROM recording_packages WHERE id=$1`, id))
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (s *RecordingPackageStore) audioTransaction(ctx context.Context, id, claim string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Match revocation/deletion: owner authority precedes every slot/package lock.
	// The identity is immutable; recheck parent eligibility after acquiring the fence.
	var recordingID string
	if err := tx.QueryRowContext(ctx, `SELECT recording_id FROM recording_packages WHERE id=$1`, id).Scan(&recordingID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return assets.ErrConflict
		}
		return err
	}
	if err := checkRecordingNotDeleted(ctx, tx, recordingID); err != nil {
		return err
	}
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>clock_timestamp() FOR UPDATE`, id, claim).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT p.id FROM recording_packages p JOIN recording_audio a ON a.package_id=p.id WHERE p.id=$1 AND `+audioAlive+` AND a.state='processing' AND a.claim_id=$2 AND a.claimed_until>clock_timestamp() FOR UPDATE OF p,a`, id, claim).Scan(&found)
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

func (s *RecordingPackageStore) HeartbeatPackageAudio(ctx context.Context, id, claim string) error {
	return s.audioTransaction(ctx, id, claim, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()+interval '2 minutes' WHERE claim_id=$1`, claim); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_audio SET claimed_until=clock_timestamp()+interval '2 minutes' WHERE package_id=$1`, id)
		return err
	})
}
func (s *RecordingPackageStore) FinishPackageAudio(ctx context.Context, id, claim string, ready bool, publish func(context.Context) error) error {
	if ready && publish == nil {
		return assets.ErrInvalidInput
	}
	return s.audioTransaction(ctx, id, claim, func(tx *sql.Tx) error {
		if ready {
			pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := publish(pubCtx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_audio SET state=CASE WHEN $2 THEN 'ready' WHEN attempts>=3 THEN 'failed' ELSE 'pending' END,claim_id=NULL,claimed_until=NULL,next_attempt_at=clock_timestamp()+interval '5 minutes' WHERE package_id=$1`, id, ready); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE claim_id=$1`, claim)
		return err
	})
}

// PlanRecordingAudio is read-only. It does not enqueue historical packages.
type RecordingAudioPlan struct {
	Packages        int64   `json:"packages"`
	InputBytes      int64   `json:"inputBytes"`
	Segments        int64   `json:"segments"`
	DurationSeconds float64 `json:"durationSeconds"`
}

func (s *RecordingPackageStore) PlanRecordingAudio(ctx context.Context) (RecordingAudioPlan, error) {
	var p RecordingAudioPlan
	err := s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(x.bytes),0),COALESCE(sum((r->>'segmentCount')::bigint),0),COALESCE(sum((r->>'durationSeconds')::double precision),0) FROM recording_packages p CROSS JOIN LATERAL (SELECT r FROM jsonb_array_elements(p.inventory->'renditions') r ORDER BY (r->>'videoBitrate')::bigint LIMIT 1) low CROSS JOIN LATERAL (SELECT sum((o->>'sizeBytes')::bigint) bytes FROM jsonb_array_elements(p.inventory->'objects') o WHERE o->>'path' LIKE (r->>'name')||'/%' AND o->>'path' NOT LIKE '%.m3u8') x WHERE `+audioEnqueueEligible).Scan(&p.Packages, &p.InputBytes, &p.Segments, &p.DurationSeconds)
	return p, err
}
