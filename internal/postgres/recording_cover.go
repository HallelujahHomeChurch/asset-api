package postgres

import (
	"context"
	"database/sql"
	"errors"
	"hhc/asset-api/internal/assets"
	"strings"
	"time"
)

type RecordingCover = assets.RecordingCover

type RecordingCoverStore struct{ db *sql.DB }

func NewRecordingCoverStore(db *sql.DB) *RecordingCoverStore { return &RecordingCoverStore{db: db} }

const coverColumns = `c.id,c.idempotency_key,c.package_id,c.recording_id,c.kind,c.state,c.mime,c.digest,COALESCE(c.claim_id,''),c.output_attempt,c.expires_at,COALESCE(c.inherited_cover_id,'')`

type coverScanner interface{ Scan(...any) error }

func scanCover(row coverScanner) (RecordingCover, error) {
	var c RecordingCover
	err := row.Scan(&c.ID, &c.OperationKey, &c.PackageID, &c.RecordingID, &c.Kind, &c.State, &c.MIME, &c.Digest, &c.ClaimID, &c.OutputAttempt, &c.ExpiresAt, &c.InheritedCoverID)
	if errors.Is(err, sql.ErrNoRows) {
		err = assets.ErrNotFound
	}
	return c, err
}

func (s *RecordingCoverStore) Create(ctx context.Context, pkg, recording, actor, key, mime, digest string) (RecordingCover, error) {
	var empty RecordingCover
	if actor == "" || len(actor) > 160 || key == "" || len(key) > 160 {
		return empty, assets.ErrInvalidInput
	}
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err := checkRecordingNotDeleted(ctx, tx, recording); err != nil {
		return empty, err
	}
	var expiry time.Time
	err = tx.QueryRowContext(ctx, `SELECT media_expires_at FROM recording_packages WHERE id=$1 AND recording_id=$2 AND state='ready' AND owner_service='hhc-web-api' AND media_expires_at>clock_timestamp() FOR SHARE`, pkg, recording).Scan(&expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, assets.ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	existing, err := scanCover(tx.QueryRowContext(ctx, `SELECT `+coverColumns+` FROM recording_covers c WHERE recording_id=$1 AND actor_id=$2 AND idempotency_key=$3`, recording, actor, key))
	if err == nil {
		if existing.Digest != digest || existing.MIME != mime || existing.PackageID != pkg {
			return empty, assets.ErrConflict
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, assets.ErrNotFound) {
		return empty, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM recording_covers WHERE recording_id=$1 AND kind='custom' AND created_at>now()-interval '24 hours'`, recording).Scan(&count); err != nil {
		return empty, err
	}
	if count >= 20 {
		return empty, assets.ErrConflict
	}
	id := newStoreID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,mime,digest,state,expires_at) VALUES($1,$2,$3,$4,$5,'custom',$6,$7,'uploading',$8)`, id, pkg, recording, actor, key, mime, digest, expiry); err != nil {
		return empty, err
	}
	c, err := scanCover(tx.QueryRowContext(ctx, `SELECT `+coverColumns+` FROM recording_covers c WHERE id=$1`, id))
	if err != nil {
		return empty, err
	}
	return c, tx.Commit()
}
func (s *RecordingCoverStore) Queue(ctx context.Context, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE recording_covers c SET state='pending' WHERE id=$1 AND state='uploading' AND created_at>now()-interval '24 hours' AND expires_at>now() AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id)`, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 1 {
		return nil
	}
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT state FROM recording_covers WHERE id=$1`, id).Scan(&state)
	if err == nil && (state == "pending" || state == "processing" || state == "ready") {
		return nil
	}
	return assets.ErrConflict
}
func (s *RecordingCoverStore) List(ctx context.Context, pkg, recording string) ([]RecordingCover, error) {
	var allowed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_packages WHERE id=$1 AND recording_id=$2 AND owner_service='hhc-web-api' AND state='ready' AND media_expires_at>now() AND NOT EXISTS(SELECT 1 FROM recording_deletions WHERE recording_id=$2))`, pkg, recording).Scan(&allowed); err != nil {
		return nil, err
	}
	if !allowed {
		return nil, assets.ErrNotFound
	}
	// Keep metadata receipts for interrupted clients until the recording expires.
	// Byte reads still reject stale uploads, even before the cleanup worker runs.
	columns := strings.Replace(coverColumns, "c.state", `CASE WHEN c.expires_at<=now() OR (c.kind='custom' AND c.created_at<=now()-interval '24 hours' AND NOT EXISTS(SELECT 1 FROM recording_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) THEN 'expired' ELSE c.state END`, 1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM recording_covers c WHERE package_id=$1 AND recording_id=$2 AND NOT (kind='auto' AND EXISTS(SELECT 1 FROM recording_covers inherited WHERE inherited.package_id=c.package_id AND inherited.kind='live-auto' AND inherited.state='ready')) ORDER BY created_at DESC`, pkg, recording)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []RecordingCover{}
	for rows.Next() {
		c, err := scanCover(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, c)
	}
	return values, rows.Err()
}
func (s *RecordingCoverStore) Get(ctx context.Context, pkg, recording, id string) (RecordingCover, error) {
	return scanCover(s.db.QueryRowContext(ctx, `SELECT `+coverColumns+` FROM recording_covers c JOIN recording_packages p ON p.id=c.package_id WHERE c.id=$1 AND c.package_id=$2 AND c.recording_id=$3 AND p.owner_service='hhc-web-api' AND p.state='ready' AND p.media_expires_at>now() AND c.state<>'expired' AND c.expires_at>now() AND (c.kind IN ('auto','live-auto') OR c.created_at>now()-interval '24 hours' OR EXISTS(SELECT 1 FROM recording_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) AND NOT EXISTS(SELECT 1 FROM recording_deletions WHERE recording_id=$3)`, id, pkg, recording))
}
func (s *RecordingCoverStore) Claim(ctx context.Context) (RecordingCover, error) {
	var empty RecordingCover
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE leased_until IS NULL OR leased_until<=clock_timestamp() ORDER BY slot FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, assets.ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_covers SET state='failed',claim_id=NULL,claimed_until=NULL WHERE state='processing' AND attempts=3 AND claimed_until<=clock_timestamp()`); err != nil {
		return empty, err
	}
	c, err := scanCover(tx.QueryRowContext(ctx, `SELECT `+coverColumns+` FROM recording_covers c JOIN recording_packages p ON p.id=c.package_id WHERE c.state IN ('pending','processing') AND c.attempts<3 AND c.next_attempt_at<=now() AND (c.claimed_until IS NULL OR c.claimed_until<=now()) AND c.expires_at>now() AND p.state='ready' AND p.media_expires_at>now() AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id) AND NOT EXISTS(SELECT 1 FROM recording_packages WHERE state IN ('freezing','validating') AND validation_attempts<3 AND next_attempt_at<=now()) AND NOT EXISTS(SELECT 1 FROM recording_sources WHERE state IN ('finalizing','queued','processing') AND processing_attempts<3 AND retry_until>now()) ORDER BY c.created_at FOR UPDATE OF c SKIP LOCKED LIMIT 1`))
	if errors.Is(err, assets.ErrNotFound) {
		if commitErr := tx.Commit(); commitErr != nil {
			return empty, commitErr
		}
		return empty, err
	}
	if err != nil {
		return empty, err
	}
	c.ClaimID = newStoreID()
	if _, err = tx.ExecContext(ctx, `UPDATE recording_covers SET state='processing',claim_id=$2,claimed_until=now()+interval '3 minutes',attempts=attempts+1 WHERE id=$1`, c.ID, c.ClaimID); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=now()+interval '3 minutes' WHERE slot=$1`, slot, c.ID, c.ClaimID); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_cover_attempts(claim_id,cover_id) VALUES($1,$2)`, c.ClaimID, c.ID); err != nil {
		return empty, err
	}
	return c, tx.Commit()
}
func (s *RecordingCoverStore) Finish(ctx context.Context, c RecordingCover, ready bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>clock_timestamp() FOR UPDATE`, c.ID, c.ClaimID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `UPDATE recording_covers c SET state=CASE WHEN $3 THEN 'ready' WHEN attempts=3 THEN 'failed' ELSE 'pending' END,output_attempt=CASE WHEN $3 THEN $2 ELSE '' END,claim_id=NULL,claimed_until=NULL,next_attempt_at=now()+interval '5 minutes' WHERE id=$1 AND claim_id=$2 AND state='processing' AND claimed_until>clock_timestamp() AND expires_at>now() AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id) AND EXISTS(SELECT 1 FROM recording_packages p WHERE p.id=c.package_id AND p.state='ready' AND p.media_expires_at>now())`, c.ID, c.ClaimID, ready)
	if err := recordingTransitionResult(r, err); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE slot=$1`, slot); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *RecordingCoverStore) Retain(ctx context.Context, pkg, recording, id, reference string) error {
	if reference == "" || len(reference) > 160 || strings.ContainsAny(reference, "\r\n") {
		return assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRecordingNotDeleted(ctx, tx, recording); err != nil {
		return err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT c.id FROM recording_covers c JOIN recording_packages p ON p.id=c.package_id WHERE c.id=$1 AND c.package_id=$2 AND c.recording_id=$3 AND c.state='ready' AND c.expires_at>now() AND p.state='ready' AND p.media_expires_at>now() AND (c.kind IN ('auto','live-auto') OR c.created_at>now()-interval '24 hours' OR EXISTS(SELECT 1 FROM recording_cover_references r WHERE r.cover_id=c.id AND r.released_at IS NULL)) FOR UPDATE OF c`, id, pkg, recording).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recording_cover_references(reference_id,cover_id) VALUES($1,$2) ON CONFLICT(reference_id) DO NOTHING`, reference, id); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT cover_id FROM recording_cover_references WHERE reference_id=$1 AND released_at IS NULL`, reference).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return assets.ErrConflict
		}
		return err
	}
	if found != id {
		return assets.ErrConflict
	}
	return tx.Commit()
}
func (s *RecordingCoverStore) Release(ctx context.Context, pkg, recording, id, reference string) error {
	if reference == "" || len(reference) > 160 || strings.ContainsAny(reference, "\r\n") {
		return assets.ErrInvalidInput
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO recording_cover_references(reference_id,cover_id,released_at) SELECT $1,c.id,now() FROM recording_covers c WHERE c.id=$2 AND c.package_id=$3 AND c.recording_id=$4 ON CONFLICT(reference_id) DO UPDATE SET released_at=COALESCE(recording_cover_references.released_at,EXCLUDED.released_at) WHERE recording_cover_references.cover_id=EXCLUDED.cover_id`, reference, id, pkg, recording)
	return err
}
