package auditoutbox

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"hhc/asset-api/internal/auditclient"
)

var ErrConflict = errors.New("audit event payload conflict")

type Store struct{ db *sql.DB }
type Row struct {
	ID, EventID string
	Payload     []byte
	PayloadHash string
	Attempts    int
}

func New(db *sql.DB) *Store { return &Store{db: db} }
func (s *Store) Enqueue(ctx context.Context, event auditclient.Event, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.EnqueueTx(ctx, tx, event, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) EnqueueTx(ctx context.Context, tx *sql.Tx, event auditclient.Event, now time.Time) error {
	payload, hash, err := event.Canonical()
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO audit_outboxes
 (id,event_id,payload,payload_hash,status,available_at,created_at,updated_at)
 VALUES ($1,$2,$3,$4,'pending',$5,$5,$5) ON CONFLICT (event_id) DO NOTHING`, uuid.NewString(), event.EventID, payload, hash, now.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return nil
	}
	var stored []byte
	var storedHash string
	if err := tx.QueryRowContext(ctx, `SELECT payload,payload_hash FROM audit_outboxes WHERE event_id=$1`, event.EventID).Scan(&stored, &storedHash); err != nil {
		return err
	}
	decoded, err := auditclient.Decode(stored)
	_, canonicalHash, canonicalErr := decoded.Canonical()
	if err != nil || canonicalErr != nil || canonicalHash != storedHash || storedHash != hash && !auditclient.EquivalentForEnqueue(decoded, event) {
		return ErrConflict
	}
	return nil
}
func (s *Store) Claim(ctx context.Context, now time.Time) (*Row, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var row Row
	err = tx.QueryRowContext(ctx, `WITH candidate AS (
 SELECT id FROM audit_outboxes WHERE status IN ('pending','processing') AND available_at <= $1
 ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE audit_outboxes o SET status='processing',attempts=o.attempts+1,available_at=$1::timestamptz + interval '60 seconds',updated_at=$1
 FROM candidate WHERE o.id=candidate.id RETURNING o.id::text,o.event_id,o.payload,o.payload_hash,o.attempts`, now.UTC()).Scan(&row.ID, &row.EventID, &row.Payload, &row.PayloadHash, &row.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &row, nil
}
func (s *Store) Complete(ctx context.Context, row Row, status, code string, availableAt, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE audit_outboxes SET status=$3,last_error=$4,available_at=$5,updated_at=$6
 WHERE id=$1::uuid AND status='processing' AND attempts=$2`, row.ID, row.Attempts, status, code, availableAt.UTC(), now.UTC())
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
func (s *Store) Stats(ctx context.Context, now time.Time) (auditclient.PipelineStats, error) {
	var stats auditclient.PipelineStats
	err := s.db.QueryRowContext(ctx, `SELECT
 count(*) FILTER (WHERE status='pending' OR (status='processing' AND available_at <= $1)),
 COALESCE(GREATEST(EXTRACT(EPOCH FROM ($1::timestamptz - min(created_at) FILTER (WHERE status='pending' OR (status='processing' AND available_at <= $1)))),0),0),
 count(*) FILTER (WHERE status='dead_letter') FROM audit_outboxes`, now.UTC()).Scan(&stats.PendingCount, &stats.OldestPendingSeconds, &stats.DeadLetterCount)
	return stats, err
}
