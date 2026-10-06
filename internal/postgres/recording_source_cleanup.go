package postgres

import (
	"context"
	"errors"
	"time"
)

const sourceCleanupEligible = `(state IN ('ready','failed','expired') OR (state='uploading' AND expires_at<=clock_timestamp()) OR
 (retry_until<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp())) OR
 EXISTS (SELECT 1 FROM recording_source_attempts a WHERE a.source_id=s.id AND a.state IN ('failed','abandoned','purged')))`

// ReconcileSources retains failed inputs for the original retry window. Ready
// HLS bytes belong to recording_packages; this only purges source Blob bytes
// and abandoned server-owned encode prefixes, never a ready package's output.
func (s *RecordingSourceStore) ReconcileSources(ctx context.Context, deleteSource func(context.Context, string, []string) error, deleteAttempt func(context.Context, string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recording_sources s WHERE cleanup_after<=clock_timestamp() AND `+sourceCleanupEligible+`
 ORDER BY cleanup_after LIMIT 10`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.WithSessionLock(ctx, id, func(ctx context.Context) error {
			q := s.locks.statements(ctx)
			// Persist before provider calls so even a timeout cannot starve later items.
			result, err := q.ExecContext(ctx, `UPDATE recording_sources SET cleanup_after=clock_timestamp()+interval '5 minutes' WHERE id=$1 AND cleanup_after<=clock_timestamp()`, id)
			if err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count == 0 {
				return err
			}
			// Expiring a live lease is forbidden; the worker's own retry-deadline
			// check and lease heartbeat fence all later writes/checkpoints.
			if _, err := q.ExecContext(ctx, `UPDATE recording_sources SET state='expired' WHERE id=$1 AND state<>'ready' AND
 ((state='uploading' AND expires_at<=clock_timestamp()) OR (retry_until<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp())))`, id); err != nil {
				return err
			}
			p, err := s.Get(ctx, id)
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `UPDATE recording_source_attempts a SET state='abandoned',finished_at=clock_timestamp() FROM recording_sources s WHERE a.source_id=s.id AND s.id=$1 AND a.state='processing' AND
 (a.claim_id IS DISTINCT FROM s.claim_id OR s.state='expired' OR (s.state='failed' AND s.claimed_until<=clock_timestamp()))`, id); err != nil {
				return err
			}
			rows, err := q.QueryContext(ctx, `SELECT claim_id, state IN ('failed','abandoned','purged') AND finished_at+interval '6 hours'<=clock_timestamp() FROM recording_source_attempts WHERE source_id=$1 ORDER BY created_at`, id)
			if err != nil {
				return err
			}
			var attempts, purge []string
			for rows.Next() {
				var attempt string
				var due bool
				if err := rows.Scan(&attempt, &due); err != nil {
					rows.Close()
					return err
				}
				attempts = append(attempts, attempt)
				if due {
					purge = append(purge, attempt)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if p.State == "ready" || p.State == "expired" {
				if err := deleteSource(ctx, id, attempts); err != nil {
					return err
				}
			}
			for _, attempt := range purge {
				if err := deleteAttempt(ctx, attempt); err != nil {
					return err
				}
				if _, err := q.ExecContext(ctx, `UPDATE recording_source_attempts SET state='purged' WHERE claim_id=$1 AND source_id=$2`, attempt, id); err != nil {
					return err
				}
			}
			// Repeat successful sweeps to catch previously signed late writes.
			_, err = q.ExecContext(ctx, `UPDATE recording_sources SET cleanup_after=clock_timestamp()+interval '24 hours' WHERE id=$1`, id)
			return err
		}); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}
