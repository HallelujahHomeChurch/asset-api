package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

type RecordingSourceClaim struct {
	Source    assets.RecordingSource
	ClaimID   string
	StartedAt time.Time
}

func (s *RecordingSourceStore) ClaimSourceProcessing(ctx context.Context) (RecordingSourceClaim, error) {
	var claim RecordingSourceClaim
	if _, err := s.db.ExecContext(ctx, `UPDATE recording_sources SET state='failed',processing_error='exhausted' WHERE state IN ('finalizing','queued','processing') AND processing_attempts=3 AND claimed_until<=clock_timestamp()`); err != nil {
		return claim, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return claim, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE leased_until IS NULL OR leased_until<=clock_timestamp() ORDER BY slot FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, assets.ErrNotFound
	}
	if err != nil {
		return claim, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM recording_sources WHERE state IN ('finalizing','queued','processing') AND processing_attempts<3 AND retry_until>clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, assets.ErrNotFound
	}
	if err != nil {
		return claim, err
	}
	claim.ClaimID = newStoreID()
	if _, err := tx.ExecContext(ctx, `UPDATE recording_source_attempts SET state='abandoned',finished_at=clock_timestamp() WHERE source_id=$1 AND state='processing'`, id); err != nil {
		return claim, err
	}
	var attempts int
	var phase string
	if err := tx.QueryRowContext(ctx, `UPDATE recording_sources SET state=CASE WHEN source_verified_at IS NULL THEN 'finalizing' ELSE 'processing' END,claim_id=$2,claimed_until=clock_timestamp()+interval '2 minutes',processing_attempts=processing_attempts+1,processing_error=NULL,copy_attempt_id=COALESCE(copy_attempt_id,$2) WHERE id=$1 RETURNING processing_attempts,clock_timestamp(),CASE WHEN source_verified_at IS NULL THEN 'source_finalization' ELSE 'encoding' END`, id, claim.ClaimID).Scan(&attempts, &claim.StartedAt, &phase); err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recording_sources SET processing_progress=$2 WHERE id=$1`, id, initialRecordingProgress(attempts, phase, claim.StartedAt)); err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=clock_timestamp()+interval '2 minutes' WHERE slot=$1`, slot, id, claim.ClaimID); err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recording_source_attempts(claim_id,source_id,state) VALUES($1,$2,'processing')`, claim.ClaimID, id); err != nil {
		return claim, err
	}
	claim.Source, err = scanRecordingSource(tx.QueryRowContext(ctx, `SELECT `+recordingSourceColumns+` FROM recording_sources WHERE id=$1`, id))
	if err != nil {
		return claim, err
	}
	return claim, tx.Commit()
}

func (s *RecordingSourceStore) sourceClaimTransaction(ctx context.Context, id, claim string, fn func(*sql.Tx, assets.RecordingSource) error) error {
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Same lock order as package validation: shared slot, then owner row.
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>clock_timestamp() FOR UPDATE`, id, claim).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	p, err := scanRecordingSource(tx.QueryRowContext(ctx, `SELECT `+recordingSourceColumns+` FROM recording_sources WHERE id=$1 AND claim_id=$2 AND state IN ('finalizing','processing') AND claimed_until>clock_timestamp() AND retry_until>clock_timestamp() FOR UPDATE`, id, claim))
	if errors.Is(err, assets.ErrNotFound) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	if err := fn(tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *RecordingSourceStore) HeartbeatSourceProcessing(ctx context.Context, id, claim string) error {
	return s.sourceClaimTransaction(ctx, id, claim, func(tx *sql.Tx, _ assets.RecordingSource) error {
		if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()+interval '2 minutes' WHERE claim_id=$1`, claim); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_sources SET claimed_until=clock_timestamp()+interval '2 minutes',processing_progress=CASE WHEN processing_progress IS NULL THEN NULL ELSE jsonb_set(processing_progress,'{heartbeatAt}',to_jsonb(clock_timestamp())) END WHERE id=$1`, id)
		return err
	})
}

func (s *RecordingSourceStore) CheckpointSourceCopy(ctx context.Context, id, claim string, copy assets.RecordingSourceCopy) error {
	return s.sourceClaimTransaction(ctx, id, claim, func(tx *sql.Tx, p assets.RecordingSource) error {
		if copy.State != "success" || copy.Key != "recording-sources/"+p.ID+"/final/"+p.CopyAttemptID+"/source" || copy.SizeBytes != p.SizeBytes || copy.ETag == "" {
			return assets.ErrInvalidInput
		}
		if p.SourceKey != "" && (p.SourceKey != copy.Key || p.SourceETag != copy.ETag) {
			return assets.ErrConflict
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_sources SET state='processing',source_key=$2,source_etag=$3,source_verified_at=COALESCE(source_verified_at,clock_timestamp()) WHERE id=$1`, id, copy.Key, copy.ETag)
		return err
	})
}

func (s *RecordingSourceStore) FailSourceProcessing(ctx context.Context, id, claim, failure string) error {
	if failure != "retry" && failure != "invalid" && failure != "copy" {
		return assets.ErrInvalidInput
	}
	return s.sourceClaimTransaction(ctx, id, claim, func(tx *sql.Tx, p assets.RecordingSource) error {
		code := failure
		if code == "copy" {
			code = "retry"
		}
		if code == "retry" && p.ProcessingAttempts >= 3 {
			code = "exhausted"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_sources SET state='failed',processing_error=$2,claim_id=NULL,claimed_until=NULL,copy_attempt_id=CASE WHEN $3 AND source_verified_at IS NULL THEN NULL ELSE copy_attempt_id END,cleanup_after=clock_timestamp() WHERE id=$1`, id, code, failure == "copy"); err != nil {
			return err
		}
		return finishSourceAttempt(ctx, tx, claim, "failed")
	})
}

func finishSourceAttempt(ctx context.Context, tx *sql.Tx, claim, state string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE recording_source_attempts SET state=$2,finished_at=clock_timestamp() WHERE claim_id=$1`, claim, state); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE claim_id=$1`, claim)
	return err
}

// Retry uses the original verified source and original seven-day deadline.
// The three-attempt budget includes crashes and operator-triggered retries.
func (s *RecordingSourceStore) Retry(ctx context.Context, id string, at time.Time) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-source-actor:' || $1,0))`, p.ActorID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE recording_sources SET state=CASE WHEN source_verified_at IS NULL THEN 'finalizing' ELSE 'queued' END,processing_error=NULL WHERE id=$1 AND state='failed' AND processing_error='retry' AND processing_attempts<3 AND retry_until>$2 AND NOT EXISTS(SELECT 1 FROM recording_sources other WHERE other.actor_id=$3 AND other.id<>$1 AND (other.state IN ('finalizing','queued','processing') OR (other.state='uploading' AND other.expires_at>clock_timestamp())))`, id, at, p.ActorID)
	if err := recordingTransitionResult(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

// The source worker must have passed the shared full package validator first.
// Ready package insertion and source projection are one fenced transaction.
func (s *RecordingSourceStore) FinishSourceProcessing(ctx context.Context, id, claim string, inventory assets.RecordingPackageInventory) error {
	size, err := assets.ValidateRecordingInventory(inventory)
	if err != nil {
		return err
	}
	if inventory.InventoryDigest == "" {
		return assets.ErrInvalidInput
	}
	data, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	return s.sourceClaimTransaction(ctx, id, claim, func(tx *sql.Tx, p assets.RecordingSource) error {
		if p.State != "processing" || p.SourceKey == "" || p.SourceVerifiedAt == nil {
			return assets.ErrConflict
		}
		// Each encode attempt uses its claim ID as its package ID, so existing
		// staging/final mechanics remain isolated without a second storage layout.
		prefix := "recordings/packages/" + claim + "/final/" + claim + "/"
		_, err := tx.ExecContext(ctx, `INSERT INTO recording_packages(id,session_id,owner_service,actor_id,recording_id,idempotency_key,state,size_bytes,inventory,created_at,expires_at,completed_at,final_prefix,ready_at,media_expires_at,retention_revision) VALUES($1,$1,'hhc-web-api',$2,$3,$4,'ready',$5,$6,$7,$8,$9,$10,now(),(SELECT CASE WHEN activated_at IS NULL THEN now()+interval '30 days' ELSE $9::timestamptz+(retention_days*interval '24 hours') END FROM recording_retention_policy WHERE singleton),(SELECT revision FROM recording_retention_policy WHERE singleton))`, claim, p.ActorID, p.RecordingID, "source-hls:"+id, size, data, p.CreatedAt, p.ExpiresAt, p.CompletedAt, prefix)
		if err != nil {
			return mapCollectionError(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recording_package_attempts(claim_id,package_id,state,finished_at) VALUES($1,$1,'ready',clock_timestamp())`, claim); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_sources SET state='ready',package_id=$2,claim_id=NULL,claimed_until=NULL,processing_error=NULL,cleanup_after=clock_timestamp() WHERE id=$1`, id, claim); err != nil {
			return err
		}
		return finishSourceAttempt(ctx, tx, claim, "ready")
	})
}
