package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

type RecordingPackageStore struct {
	db    *sql.DB
	locks *recordingSessionStore
}

func NewRecordingPackageStore(db *sql.DB) *RecordingPackageStore {
	return &RecordingPackageStore{db: db, locks: newRecordingSessionStore(db)}
}

func (s *RecordingPackageStore) WithSessionLock(ctx context.Context, id string, fn func(context.Context) error) error {
	return s.locks.WithSessionLock(ctx, "package-"+id, fn)
}

const recordingPackageColumns = `id,session_id,owner_service,actor_id,recording_id,idempotency_key,state,size_bytes,created_at,expires_at,inventory`
const recordingPackageSelectColumns = recordingPackageColumns + `,COALESCE(final_prefix,''),ready_at,media_expires_at,completed_at,retention_revision`

func scanRecordingPackage(row *sql.Row) (assets.RecordingPackage, error) {
	var p assets.RecordingPackage
	var inventory []byte
	err := row.Scan(&p.ID, &p.SessionID, &p.OwnerService, &p.ActorID, &p.RecordingID, &p.IdempotencyKey, &p.State, &p.SizeBytes, &p.CreatedAt, &p.ExpiresAt, &inventory, &p.FinalPrefix, &p.ReadyAt, &p.MediaExpiresAt, &p.UploadedAt, &p.RetentionRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return p, assets.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(inventory, &p.Inventory)
	return p, err
}

func (s *RecordingPackageStore) Get(ctx context.Context, id string) (assets.RecordingPackage, error) {
	return scanRecordingPackage(s.locks.statements(ctx).QueryRowContext(ctx, `SELECT `+recordingPackageSelectColumns+` FROM recording_packages WHERE id=$1 AND EXISTS(SELECT 1 FROM recording_retention_policy WHERE singleton)`, id))
}

func (s *RecordingPackageStore) FindByIdempotency(ctx context.Context, key string) (assets.RecordingPackage, error) {
	return scanRecordingPackage(s.locks.statements(ctx).QueryRowContext(ctx, `SELECT `+recordingPackageSelectColumns+` FROM recording_packages WHERE idempotency_key=$1`, key))
}

func (s *RecordingPackageStore) Create(ctx context.Context, p assets.RecordingPackage) error {
	size, err := assets.ValidateRecordingInventory(p.Inventory)
	if err != nil {
		return err
	}
	if p.Inventory.InventoryDigest == "" || p.SizeBytes != size || p.State != "uploading" {
		return assets.ErrInvalidInput
	}
	data, err := json.Marshal(p.Inventory)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRecordingNotDeleted(ctx, tx, p.RecordingID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-package-actor:' || $1,0))`, p.ActorID); err != nil {
		return err
	}
	var active int
	// An accepted freeze/validation remains active after the upload URL expires.
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recording_packages WHERE actor_id=$1 AND (state IN ('freezing','validating') OR (state='uploading' AND expires_at > now()))`, p.ActorID).Scan(&active)
	if err != nil {
		return err
	}
	if active >= 2 {
		return assets.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_packages (`+recordingPackageColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, p.ID, p.SessionID, p.OwnerService, p.ActorID, p.RecordingID, p.IdempotencyKey, p.State, p.SizeBytes, p.CreatedAt, p.ExpiresAt, data)
	if err != nil {
		return mapCollectionError(err)
	}
	return tx.Commit()
}

// This row is the durable job receipt. The worker, not this request, freezes
// and verifies the object graph before committing a ready package.
func (s *RecordingPackageStore) Freeze(ctx context.Context, id string, at time.Time) error {
	result, err := s.locks.statements(ctx).ExecContext(ctx, `UPDATE recording_packages SET state='freezing',completed_at=$2 WHERE id=$1 AND state='uploading' AND expires_at > $2`, id, at)
	return recordingTransitionResult(result, err)
}

var _ assets.RecordingPackageRepository = (*RecordingPackageStore)(nil)

type RecordingPackageClaim struct {
	Package   assets.RecordingPackage
	ClaimID   string
	Attempts  int
	StartedAt time.Time
}

func (s *RecordingPackageStore) ClaimPackageValidation(ctx context.Context) (RecordingPackageClaim, error) {
	var claim RecordingPackageClaim
	// Do not hold package locks while waiting for a slot: all lease transactions
	// below acquire slot then package, preventing heartbeat/claim lock inversion.
	if _, err := s.db.ExecContext(ctx, `UPDATE recording_packages SET state='failed',validation_error='exhausted' WHERE state='validating' AND validation_attempts=3 AND claimed_until<=clock_timestamp()`); err != nil {
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
	err = tx.QueryRowContext(ctx, `SELECT id FROM recording_packages WHERE state IN ('freezing','validating') AND validation_attempts<3 AND next_attempt_at<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return claim, err
		}
		return claim, assets.ErrNotFound
	}
	if err != nil {
		return claim, err
	}
	claim.ClaimID = newStoreID()
	if _, err := tx.ExecContext(ctx, `UPDATE recording_package_attempts SET state='abandoned',finished_at=clock_timestamp() WHERE package_id=$1 AND state='processing'`, id); err != nil {
		return claim, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE recording_packages SET state='validating',claim_id=$2,claimed_until=clock_timestamp()+interval '2 minutes',validation_attempts=validation_attempts+1,validation_error=NULL WHERE id=$1 RETURNING validation_attempts,clock_timestamp()`, id, claim.ClaimID).Scan(&claim.Attempts, &claim.StartedAt)
	if err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET processing_progress=$2 WHERE id=$1`, id, initialRecordingProgress(claim.Attempts, "package_validation", claim.StartedAt)); err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=clock_timestamp()+interval '2 minutes' WHERE slot=$1`, slot, id, claim.ClaimID); err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recording_package_attempts(claim_id,package_id,state) VALUES ($1,$2,'processing')`, claim.ClaimID, id); err != nil {
		return claim, err
	}
	claim.Package, err = scanRecordingPackage(tx.QueryRowContext(ctx, `SELECT `+recordingPackageSelectColumns+` FROM recording_packages WHERE id=$1`, id))
	if err != nil {
		return claim, err
	}
	return claim, tx.Commit()
}

// Lock slot then package in the same order as claim. Expired workers cannot
// revive their lease, extend a new worker's slot, or commit a stale ready state.
func (s *RecordingPackageStore) packageClaimTransaction(ctx context.Context, id, claimID string, fn func(*sql.Tx) error) error {
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>clock_timestamp() FOR UPDATE`, id, claimID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	var locked string
	err = tx.QueryRowContext(ctx, `SELECT id FROM recording_packages WHERE id=$1 AND claim_id=$2 AND state='validating' AND claimed_until>clock_timestamp() FOR UPDATE`, id, claimID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *RecordingPackageStore) HeartbeatPackageValidation(ctx context.Context, id, claimID string) error {
	return s.packageClaimTransaction(ctx, id, claimID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()+interval '2 minutes' WHERE claim_id=$1`, claimID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_packages SET claimed_until=clock_timestamp()+interval '2 minutes',processing_progress=CASE WHEN processing_progress IS NULL THEN NULL ELSE jsonb_set(processing_progress,'{heartbeatAt}',to_jsonb(clock_timestamp())) END WHERE id=$1`, id)
		return err
	})
}

func (s *RecordingPackageStore) FinishPackageValidation(ctx context.Context, id, claimID string, ready bool, failure string) error {
	if ready && failure != "" || !ready && failure != "invalid" && failure != "retry" {
		return assets.ErrInvalidInput
	}
	return s.packageClaimTransaction(ctx, id, claimID, func(tx *sql.Tx) error {
		if ready {
			prefix := "recordings/packages/" + id + "/final/" + claimID + "/"
			if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET state='ready',final_prefix=$2,ready_at=now(),media_expires_at=(SELECT CASE WHEN activated_at IS NULL THEN now()+interval '30 days' ELSE completed_at+(retention_days*interval '24 hours') END FROM recording_retention_policy WHERE singleton),retention_revision=(SELECT revision FROM recording_retention_policy WHERE singleton),claim_id=NULL,claimed_until=NULL,validation_error=NULL,cleanup_after=now() WHERE id=$1`, id, prefix); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,state,expires_at) SELECT $2,id,recording_id,actor_id,'auto','auto','pending',media_expires_at FROM recording_packages WHERE id=$1 ON CONFLICT DO NOTHING`, id, newStoreID()); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET state=CASE WHEN $2='invalid' OR validation_attempts=3 THEN 'failed' ELSE 'validating' END,validation_error=$2,claim_id=NULL,claimed_until=NULL,next_attempt_at=clock_timestamp()+interval '5 minutes',cleanup_after=now() WHERE id=$1`, id, failure); err != nil {
				return err
			}
		}
		state := "failed"
		if ready {
			state = "ready"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_package_attempts SET state=$2,finished_at=clock_timestamp() WHERE claim_id=$1`, claimID, state); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE claim_id=$1`, claimID)
		return err
	})
}
