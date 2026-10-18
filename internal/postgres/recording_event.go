package postgres

import (
	"context"
	"database/sql"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/recordingqueue"
	"time"
)

type RecordingEventStore struct {
	db      *sql.DB
	sources bool
	ready   func(context.Context, string) (bool, error)
}

func NewRecordingEventStore(db *sql.DB, sources bool) *RecordingEventStore {
	s := &RecordingEventStore{db: db, sources: sources}
	s.ready = s.workReady
	return s
}
func (s *RecordingEventStore) workReady(ctx context.Context, lane string) (bool, error) {
	var query string
	switch lane {
	case "live":
		query = liveWakeCandidate
	case "processing":
		query = `SELECT 1 WHERE EXISTS (` + packageWakeCandidate + `) OR EXISTS (SELECT 1 FROM recording_packages WHERE state='validating' AND validation_attempts=3 AND claimed_until<=now())`
		if s.sources {
			query = `SELECT 1 WHERE EXISTS (` + sourceWakeCandidate + `) OR EXISTS (` + query + `) OR EXISTS (SELECT 1 FROM recording_sources WHERE state IN ('finalizing','queued','processing') AND processing_attempts=3 AND claimed_until<=now())`
		}
	case "preview":
		query = `SELECT 1 WHERE EXISTS (` + liveCoverWakeCandidate + `) OR EXISTS (` + coverWakeCandidate + `) OR EXISTS (` + previewWakeCandidate + `) OR EXISTS (` + livePreviewWakeCandidate + `) OR EXISTS (SELECT 1 FROM recording_live l JOIN recording_captures c ON c.id=l.capture_id WHERE c.state='uploading' AND c.expires_at>now() AND l.published_sequence>=0 AND NOT EXISTS(SELECT 1 FROM recording_live_previews p WHERE p.capture_id=l.capture_id) AND ` + previewWorkWaiting + `)`
		query = `SELECT 1 WHERE EXISTS (` + query + `) OR EXISTS (SELECT 1 FROM recording_live_covers WHERE state='processing' AND attempts=3 AND claimed_until<=now()) OR EXISTS (SELECT 1 FROM recording_covers WHERE state='processing' AND attempts=3 AND claimed_until<=now()) OR EXISTS (SELECT 1 FROM recording_packages WHERE preview_state='processing' AND preview_attempts=3 AND preview_claimed_until<=now())`
	default:
		return false, assets.ErrInvalidInput
	}
	var ready bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (`+query+`)`).Scan(&ready)
	return ready, err
}
func (s *RecordingEventStore) ClaimDispatch(ctx context.Context) (recordingqueue.Event, bool, error) {
	var event recordingqueue.Event
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event, false, err
	}
	defer tx.Rollback()
	// Admission is separate from media slots: it never reserves a slot ahead of a claim.
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('recording-event-dispatch',0))`).Scan(&locked); err != nil || !locked {
		return event, false, err
	}
	var occupied int
	if err = tx.QueryRowContext(ctx, `SELECT GREATEST((SELECT count(*) FROM recording_event_wakes WHERE state<>'idle' AND lease_until>now()),(SELECT count(*) FROM recording_processing_slots WHERE leased_until>now()))`).Scan(&occupied); err != nil || occupied >= 2 {
		return event, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT lane,generation FROM recording_event_wakes WHERE (state='idle' OR lease_until<=now()) AND next_check_at<=now() ORDER BY CASE WHEN lane='live' THEN 0 ELSE 1 END,last_dispatch_at NULLS FIRST,lane`)
	if err != nil {
		return event, false, err
	}
	var candidates []recordingqueue.Event
	for rows.Next() {
		var e recordingqueue.Event
		if err = rows.Scan(&e.Lane, &e.Generation); err != nil {
			rows.Close()
			return event, false, err
		}
		candidates = append(candidates, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return event, false, err
	}
	for _, e := range candidates {
		ready, err := s.ready(ctx, e.Lane)
		if err != nil {
			return event, false, err
		}
		if !ready {
			// Timed retries and lost leases become eligible without waiting for hourly maintenance.
			if _, err = tx.ExecContext(ctx, `UPDATE recording_event_wakes SET next_check_at=now()+interval '10 seconds' WHERE lane=$1 AND generation=$2`, e.Lane, e.Generation); err != nil {
				return event, false, err
			}
			continue
		}
		e.Type = "asset.recording.wake.v1"
		e.Version = 1
		e.Token = newStoreID()
		// A transaction that changed the generation since the probe wins; retry the scan.
		r, err := tx.ExecContext(ctx, `UPDATE recording_event_wakes SET state='dispatching',token=$3,dispatch_generation=$2,lease_until=now()+interval '2 minutes',last_dispatch_at=now() WHERE lane=$1 AND generation=$2 AND (state='idle' OR lease_until<=now())`, e.Lane, e.Generation, e.Token)
		if err != nil {
			return event, false, err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			continue
		}
		if err = tx.Commit(); err != nil {
			return event, false, err
		}
		return e, true, nil
	}
	return event, false, tx.Commit()
}
func (s *RecordingEventStore) Sent(ctx context.Context, e recordingqueue.Event) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recording_event_wakes SET state='queued' WHERE lane=$1 AND token=$2 AND dispatch_generation=$3 AND state='dispatching'`, e.Lane, e.Token, e.Generation)
	return err
}
func (s *RecordingEventStore) SendFailed(ctx context.Context, e recordingqueue.Event) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recording_event_wakes SET state='idle',token=NULL,dispatch_generation=NULL,lease_until=NULL,next_check_at=now()+interval '15 seconds' WHERE lane=$1 AND token=$2 AND dispatch_generation=$3 AND state='dispatching'`, e.Lane, e.Token, e.Generation)
	return err
}
func (s *RecordingEventStore) Begin(ctx context.Context, e recordingqueue.Event) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE recording_event_wakes SET state='running',lease_until=now()+interval '2 minutes' WHERE lane=$1 AND token=$2 AND dispatch_generation=$3 AND state IN ('dispatching','queued') AND lease_until>now()`, e.Lane, e.Token, e.Generation)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *RecordingEventStore) Renew(ctx context.Context, e recordingqueue.Event) error {
	r, err := s.db.ExecContext(ctx, `UPDATE recording_event_wakes SET lease_until=now()+interval '2 minutes' WHERE lane=$1 AND token=$2 AND dispatch_generation=$3 AND state='running' AND lease_until>now()`, e.Lane, e.Token, e.Generation)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return assets.ErrConflict
	}
	return nil
}
func (s *RecordingEventStore) Finish(ctx context.Context, e recordingqueue.Event, success bool) error {
	delay := time.Duration(0)
	if !success {
		delay = 15 * time.Second
	}
	r, err := s.db.ExecContext(ctx, `UPDATE recording_event_wakes SET state='idle',token=NULL,dispatch_generation=NULL,lease_until=NULL,next_check_at=now()+$4::interval WHERE lane=$1 AND token=$2 AND dispatch_generation=$3 AND state='running' AND lease_until>now()`, e.Lane, e.Token, e.Generation, delay.String())
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return assets.ErrConflict
	}
	return nil
}
func (s *RecordingEventStore) Sweep(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recording_event_wakes SET next_check_at=LEAST(next_check_at,now()) WHERE state='idle' OR lease_until<=now()`)
	return err
}

var _ recordingqueue.Repository = (*RecordingEventStore)(nil)
