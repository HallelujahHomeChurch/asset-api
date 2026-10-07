package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
	"time"
)

type RecordingCaptureStore struct{ db *sql.DB }

func NewRecordingCaptureStore(db *sql.DB) *RecordingCaptureStore { return &RecordingCaptureStore{db} }

const captureColumns = `id,actor_id,recording_id,create_key,state,created_at,expires_at,declared_bytes,declared_objects,receipts,package_id,inventory,terminal_at,terminal_reason`

func scanCapture(row *sql.Row) (assets.RecordingCapture, error) {
	var c assets.RecordingCapture
	var receipts, inv []byte
	err := row.Scan(&c.ID, &c.ActorID, &c.RecordingID, &c.CreateKey, &c.State, &c.CreatedAt, &c.ExpiresAt, &c.DeclaredBytes, &c.DeclaredObjects, &receipts, &c.PackageID, &inv, &c.TerminalAt, &c.TerminalReason)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(receipts, &c.Receipts); err != nil {
		return c, err
	}
	if len(inv) > 0 {
		err = json.Unmarshal(inv, &c.Inventory)
	}
	return c, err
}
func readCaptureObjects(ctx context.Context, tx *sql.Tx, c *assets.RecordingCapture) error {
	rows, err := tx.QueryContext(ctx, `SELECT path,size_bytes,sha256,state FROM recording_capture_objects WHERE capture_id=$1 ORDER BY path`, c.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	c.Objects = []assets.RecordingCaptureObject{}
	for rows.Next() {
		var o assets.RecordingCaptureObject
		if err = rows.Scan(&o.Path, &o.SizeBytes, &o.SHA256, &o.State); err != nil {
			return err
		}
		c.Objects = append(c.Objects, o)
	}
	return rows.Err()
}
func (s *RecordingCaptureStore) CreateCapture(ctx context.Context, c assets.RecordingCapture) (assets.RecordingCapture, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	if err = checkRecordingNotDeleted(ctx, tx, c.RecordingID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-package-actor:' || $1,0))`, c.ActorID); err != nil {
		return c, err
	}
	old, err := scanCapture(tx.QueryRowContext(ctx, `SELECT `+captureColumns+` FROM recording_captures WHERE create_key=$1`, c.CreateKey))
	if err == nil {
		if old.ActorID != c.ActorID || old.RecordingID != c.RecordingID {
			return c, assets.ErrConflict
		}
		if err = readCaptureObjects(ctx, tx, &old); err != nil {
			return c, err
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, assets.ErrNotFound) {
		return c, err
	}
	var active int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM recording_captures WHERE actor_id=$1 AND ((package_id IS NOT NULL AND EXISTS(SELECT 1 FROM recording_packages p WHERE p.id=recording_captures.package_id AND p.state IN ('freezing','validating'))) OR (package_id IS NULL AND state='uploading' AND expires_at>clock_timestamp())))+(SELECT count(*) FROM recording_packages WHERE actor_id=$1 AND (state IN ('freezing','validating') OR (state='uploading' AND expires_at>clock_timestamp())) AND NOT EXISTS(SELECT 1 FROM recording_captures c WHERE c.package_id=recording_packages.id))`, c.ActorID).Scan(&active)
	if err != nil {
		return c, err
	}
	if active >= 2 {
		return c, assets.ErrConflict
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_packages WHERE recording_id=$1 UNION ALL SELECT 1 FROM recording_sources WHERE recording_id=$1)`, c.RecordingID).Scan(&exists)
	if err != nil {
		return c, err
	}
	if exists {
		return c, assets.ErrConflict
	}
	receipts, err := json.Marshal(c.Receipts)
	if err != nil {
		return c, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,receipts) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, c.ID, c.ActorID, c.RecordingID, c.CreateKey, c.State, c.CreatedAt, c.ExpiresAt, receipts)
	if err != nil {
		return c, mapCollectionError(err)
	}
	return c, tx.Commit()
}
func (s *RecordingCaptureStore) GetCapture(ctx context.Context, id string) (assets.RecordingCapture, error) {
	return s.UpdateCapture(ctx, id, func(*assets.RecordingCapture) error { return nil })
}
func (s *RecordingCaptureStore) UpdateCapture(ctx context.Context, id string, fn func(*assets.RecordingCapture) error) (assets.RecordingCapture, error) {
	var c assets.RecordingCapture
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	// Owner lock fences CMS deletion before taking the capture row lock. No
	// network/provider operation can race a tombstone or a competing seal.
	var recording string
	err = tx.QueryRowContext(ctx, `SELECT recording_id FROM recording_captures WHERE id=$1`, id).Scan(&recording)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recording-owner:' || $1,0))`, recording); err != nil {
		return c, err
	}
	c, err = scanCapture(tx.QueryRowContext(ctx, `SELECT `+captureColumns+` FROM recording_captures WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return c, err
	}
	if err = readCaptureObjects(ctx, tx, &c); err != nil {
		return c, err
	}
	if c.PackageID != nil && c.State != "aborted" {
		var state string
		err = tx.QueryRowContext(ctx, `SELECT state FROM recording_packages WHERE id=$1`, *c.PackageID).Scan(&state)
		if err != nil {
			return c, err
		}
		c.State = state
		if state == "ready" {
			// Full package validation proves every declared object, unlike confirm's
			// size evidence. Keep the resumable object projection consistent.
			if _, err = tx.ExecContext(ctx, `UPDATE recording_capture_objects SET state='verified' WHERE capture_id=$1 AND state<>'verified'`, c.ID); err != nil {
				return c, err
			}
			for i := range c.Objects {
				c.Objects[i].State = "verified"
			}
		}

		if state == "failed" || state == "expired" {
			at := time.Now().UTC()
			reason := "package_" + state
			if c.TerminalAt == nil {
				c.TerminalAt = &at
				c.TerminalReason = &reason
			}
		}
	}
	var deleted bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_deletions WHERE recording_id=$1)`, recording).Scan(&deleted); err != nil {
		return c, err
	}
	if deleted {
		c.State = "expired"
		at := time.Now().UTC()
		reason := "recording_deleted"
		if c.TerminalAt == nil {
			c.TerminalAt = &at
			c.TerminalReason = &reason
		}
	}
	if (c.State == "uploading" || c.State == "freezing" || c.State == "validating") && !time.Now().Before(c.ExpiresAt) {
		c.State = "expired"
		at := c.ExpiresAt
		reason := "capture_expired"
		c.TerminalAt = &at
		c.TerminalReason = &reason
	}
	previousObjects := make(map[string]assets.RecordingCaptureObject, len(c.Objects))
	for _, o := range c.Objects {
		previousObjects[o.Path] = o
	}
	previousPackage := c.PackageID
	if err = fn(&c); err != nil {
		return c, err
	}
	if previousPackage == nil && c.PackageID != nil {
		if c.Inventory == nil || c.State != "freezing" {
			return c, assets.ErrInvalidInput
		}
		size, err := assets.ValidateRecordingInventory(*c.Inventory)
		if err != nil {
			return c, err
		}
		data, err := json.Marshal(c.Inventory)
		if err != nil {
			return c, err
		}
		var accepted time.Time
		for _, r := range c.Receipts {
			if r.Receipt.Operation == "seal" {
				accepted = r.Receipt.AcceptedAt
			}
		}
		if accepted.IsZero() {
			return c, assets.ErrInvalidInput
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO recording_packages(`+recordingPackageColumns+`,completed_at) VALUES($1,$1,'hhc-web-api',$2,$3,$4,'freezing',$5,$6,$7,$8,$9)`, c.ID, c.ActorID, c.RecordingID, "capture:"+c.ID, size, c.CreatedAt, c.ExpiresAt, data, accepted)
		if err != nil {
			return c, mapCollectionError(err)
		}
	}
	if c.PackageID != nil && (c.State == "aborted" || c.State == "expired" || c.State == "failed") {
		if _, err = tx.ExecContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 FOR UPDATE`, c.ID); err != nil {
			return c, err
		}
		// Validation may have committed while this transaction waited for the
		// slot. Lock the package second and recheck before committing a terminal
		// receipt; an earlier ready commit must never coexist with abort.
		var authoritativeState string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM recording_packages WHERE id=$1 FOR UPDATE`, c.ID).Scan(&authoritativeState); err != nil {
			return c, err
		}
		if authoritativeState == "ready" {
			return c, assets.ErrConflict
		}
		state := "failed"
		if c.State == "expired" {
			state = "expired"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recording_packages SET state=$2,claim_id=NULL,claimed_until=NULL,cleanup_after=clock_timestamp() WHERE id=$1 AND state <> 'ready'`, c.ID, state); err != nil {
			return c, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recording_package_attempts SET state='abandoned',finished_at=clock_timestamp() WHERE package_id=$1 AND state='processing'`, c.ID); err != nil {
			return c, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE job_id=$1`, c.ID); err != nil {
			return c, err
		}
	}
	receipts, err := json.Marshal(c.Receipts)
	if err != nil {
		return c, err
	}
	var inventory any
	if c.Inventory != nil {
		data, err := json.Marshal(c.Inventory)
		if err != nil {
			return c, err
		}
		inventory = data
	}
	_, err = tx.ExecContext(ctx, `UPDATE recording_captures SET state=$2,declared_bytes=$3,declared_objects=$4,receipts=$5,package_id=$6,inventory=$7,terminal_at=$8,terminal_reason=$9 WHERE id=$1`, id, c.State, c.DeclaredBytes, c.DeclaredObjects, receipts, c.PackageID, inventory, c.TerminalAt, c.TerminalReason)
	if err != nil {
		return c, err
	}
	for _, o := range c.Objects {
		if old, ok := previousObjects[o.Path]; ok && old == o {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO recording_capture_objects(capture_id,path,size_bytes,sha256,state) VALUES($1,$2,$3,$4,$5) ON CONFLICT(capture_id,path) DO UPDATE SET state=EXCLUDED.state WHERE recording_capture_objects.size_bytes=EXCLUDED.size_bytes AND recording_capture_objects.sha256=EXCLUDED.sha256`, id, o.Path, o.SizeBytes, o.SHA256, o.State)
		if err != nil {
			return c, err
		}
	}
	return c, tx.Commit()
}

var _ assets.RecordingCaptureRepository = (*RecordingCaptureStore)(nil)
