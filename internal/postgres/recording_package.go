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
	locks *RecordingUploadStore
}

func NewRecordingPackageStore(db *sql.DB) *RecordingPackageStore {
	return &RecordingPackageStore{db: db, locks: NewRecordingUploadStore(db)}
}

func (s *RecordingPackageStore) WithSessionLock(ctx context.Context, id string, fn func(context.Context) error) error {
	return s.locks.WithSessionLock(ctx, "package-"+id, fn)
}

const recordingPackageColumns = `id,session_id,owner_service,actor_id,recording_id,idempotency_key,state,size_bytes,created_at,expires_at,inventory`

func scanRecordingPackage(row *sql.Row) (assets.RecordingPackage, error) {
	var p assets.RecordingPackage
	var inventory []byte
	err := row.Scan(&p.ID, &p.SessionID, &p.OwnerService, &p.ActorID, &p.RecordingID, &p.IdempotencyKey, &p.State, &p.SizeBytes, &p.CreatedAt, &p.ExpiresAt, &inventory)
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
	return scanRecordingPackage(s.locks.statements(ctx).QueryRowContext(ctx, `SELECT `+recordingPackageColumns+` FROM recording_packages WHERE id=$1`, id))
}

func (s *RecordingPackageStore) FindByIdempotency(ctx context.Context, key string) (assets.RecordingPackage, error) {
	return scanRecordingPackage(s.locks.statements(ctx).QueryRowContext(ctx, `SELECT `+recordingPackageColumns+` FROM recording_packages WHERE idempotency_key=$1`, key))
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
