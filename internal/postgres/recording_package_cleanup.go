package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const packageCleanupLimit = 10

const packageCleanupEligibleAt = `(state IN ('ready','failed','expired') OR (state='uploading' AND expires_at<=%[1]s) OR
 EXISTS (SELECT 1 FROM recording_package_attempts a WHERE a.package_id=p.id AND a.state IN ('failed','abandoned','purged')))
 AND NOT EXISTS(SELECT 1 FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.package_id=p.id AND NOT (` + captureCleanupSafeAt + `))`

var packageCleanupEligible = fmt.Sprintf(packageCleanupEligibleAt, "clock_timestamp()")

func (s *RecordingPackageStore) PackageCleanupCandidates(ctx context.Context) ([]string, error) {
	ids, _, err := s.packageCleanupCandidates(ctx)
	return ids, err
}

func (s *RecordingPackageStore) packageCleanupCandidates(ctx context.Context) ([]string, time.Time, error) {
	if err := NewRecordingCaptureStore(s.db).reconcileTerminalPackages(ctx); err != nil {
		return nil, time.Time{}, err
	}
	// Filter protected captures before LIMIT. Keep the database's cutoff even
	// for an empty batch so the remaining count uses the same eligibility clock.
	eligible := fmt.Sprintf(packageCleanupEligibleAt, "statement_timestamp()")
	rows, err := s.db.QueryContext(ctx, `WITH batch AS (
 SELECT p.id, p.cleanup_after FROM recording_packages p WHERE cleanup_after<=statement_timestamp() AND `+eligible+`
 ORDER BY cleanup_after LIMIT $1
 )
 SELECT batch.id, snapshot.cutoff FROM (SELECT statement_timestamp() AS cutoff) snapshot
 LEFT JOIN batch ON true ORDER BY batch.cleanup_after`, packageCleanupLimit)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var ids []string
	var cutoff time.Time
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id, &cutoff); err != nil {
			return nil, time.Time{}, err
		}
		if id.Valid {
			ids = append(ids, id.String)
		}
	}
	return ids, cutoff, rows.Err()
}

// Cleanup uses the same session lock as URL issuance and only deletes declared
// object paths. Failed deletion is retried; successful sweeps are repeated to
// remove late writes made with capabilities issued before complete/expiry.
func (s *RecordingPackageStore) CleanupPackage(ctx context.Context, id string, deleteObjects func(context.Context, []string) error) error {
	_, err := s.cleanupPackage(ctx, id, deleteObjects)
	return err
}

func (s *RecordingPackageStore) cleanupPackage(ctx context.Context, id string, deleteObjects func(context.Context, []string) error) (bool, error) {
	var swept bool
	err := s.locks.WithSessionLock(ctx, "package-"+id, func(ctx context.Context, conn *sql.Conn) error {
		p, err := (recordingPackageSession{query: conn}).Get(ctx, id)
		if err != nil {
			return err
		}
		var uploadExpired, mediaExpired, gracePassed, due bool
		err = conn.QueryRowContext(ctx, `SELECT expires_at<=clock_timestamp(),COALESCE(media_expires_at<=clock_timestamp(),false),COALESCE(GREATEST(media_expires_at+interval '1 hour',grant_cleanup_after)<=clock_timestamp(),false),cleanup_after<=clock_timestamp() FROM recording_packages WHERE id=$1`, id).Scan(&uploadExpired, &mediaExpired, &gracePassed, &due)
		if err != nil {
			return err
		}
		if !due {
			return nil
		}
		var captureProtected bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.package_id=$1 AND NOT (`+captureCleanupSafe+`))`, id).Scan(&captureProtected); err != nil {
			return err
		}
		if captureProtected {
			return nil
		}
		// Persist before provider calls so even a timeout cannot starve later items.
		if _, err := conn.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=clock_timestamp()+interval '5 minutes' WHERE id=$1`, id); err != nil {
			return err
		}
		if p.State == "uploading" && uploadExpired || p.State == "ready" && mediaExpired {
			if _, err := conn.ExecContext(ctx, `UPDATE recording_packages SET state='expired' WHERE id=$1 AND state=$2`, id, p.State); err != nil {
				return err
			}
			p.State = "expired"
		}
		deletePrefix := func(prefix string, control bool) error {
			keys := make([]string, 0, 1000)
			for _, o := range p.Inventory.Objects {
				keys = append(keys, prefix+o.Path)
				if len(keys) == 1000 {
					if err := deleteObjects(ctx, keys); err != nil {
						return err
					}
					keys = keys[:0]
				}
			}
			if control {
				keys = append(keys, prefix+"package.json")
			}
			if len(keys) > 0 {
				return deleteObjects(ctx, keys)
			}
			return nil
		}
		if p.State == "ready" || p.State == "failed" || p.State == "expired" {
			if err := deletePrefix("recordings/packages/"+id+"/staging/", false); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `UPDATE recording_package_attempts a SET state='abandoned',finished_at=clock_timestamp() FROM recording_packages p WHERE a.package_id=p.id AND p.id=$1 AND a.state='processing' AND (a.claim_id IS DISTINCT FROM p.claim_id OR (p.state='failed' AND p.claimed_until<=clock_timestamp()))`, id); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, `SELECT claim_id FROM recording_package_attempts WHERE package_id=$1 AND
 ((state IN ('failed','abandoned','purged') AND finished_at+interval '6 hours'<=clock_timestamp()) OR (state='ready' AND $2))`, id, gracePassed)
		if err != nil {
			return err
		}
		var attempts []string
		for rows.Next() {
			var attempt string
			if err := rows.Scan(&attempt); err != nil {
				rows.Close()
				return err
			}
			attempts = append(attempts, attempt)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, attempt := range attempts {
			if err := deletePrefix("recordings/packages/"+id+"/final/"+attempt+"/", true); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `UPDATE recording_package_attempts SET state='purged' WHERE claim_id=$1 AND package_id=$2`, attempt, id); err != nil {
				return err
			}
		}
		// At most three preview attempts per package. Preserve all attempts
		// until media expiry + grant grace, including an ambiguous successful
		// pointer publication. Repeat declared-key sweeps to catch late writes.
		if gracePassed && p.FinalPrefix != "" {
			rows, err := conn.QueryContext(ctx, `SELECT claim_id FROM recording_preview_attempts WHERE package_id=$1`, id)
			if err != nil {
				return err
			}
			var previews []string
			for rows.Next() {
				var claim string
				if err := rows.Scan(&claim); err != nil {
					rows.Close()
					return err
				}
				previews = append(previews, claim)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, claim := range previews {
				keys := []string{p.FinalPrefix + "previews/" + claim + "/index.vtt"}
				for i := 0; i < p.Inventory.Renditions[0].SegmentCount; i++ {
					keys = append(keys, fmt.Sprintf("%spreviews/%s/seg-%06d.jpg", p.FinalPrefix, claim, i))
					if len(keys) == 1000 {
						if err := deleteObjects(ctx, keys); err != nil {
							return err
						}
						keys = nil
					}
				}
				if len(keys) > 0 {
					if err := deleteObjects(ctx, keys); err != nil {
						return err
					}
				}
			}
			if err := deleteObjects(ctx, []string{p.FinalPrefix + "previews/current.json"}); err != nil {
				return err
			}
		}

		// Keep bounded attempts through grant grace, including ambiguous pointer
		// publication, then repeat the declared-key sweep to catch late writes.
		if gracePassed && p.FinalPrefix != "" {
			rows, err := conn.QueryContext(ctx, `SELECT claim_id FROM recording_audio_attempts WHERE package_id=$1`, id)
			if err != nil {
				return err
			}
			var attempts []string
			for rows.Next() {
				var claim string
				if err := rows.Scan(&claim); err != nil {
					rows.Close()
					return err
				}
				attempts = append(attempts, claim)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, claim := range attempts {
				prefix := p.FinalPrefix + "audio/aac-hls-v1/" + claim + "/"
				keys := []string{prefix + "index.m3u8"}
				for i := 0; i < p.Inventory.Renditions[0].SegmentCount; i++ {
					keys = append(keys, fmt.Sprintf("%sseg-%06d.ts", prefix, i))
					if len(keys) == 1000 {
						if err := deleteObjects(ctx, keys); err != nil {
							return err
						}
						keys = nil
					}
				}
				if len(keys) > 0 {
					if err := deleteObjects(ctx, keys); err != nil {
						return err
					}
				}
			}
			if err := deleteObjects(ctx, []string{p.FinalPrefix + "audio/aac-hls-v1/current.json"}); err != nil {
				return err
			}
		}
		// ponytail: daily declared-key sweep retains metadata; add bounded metadata
		// compaction only after retention policy and provider late-write bounds agree.
		_, err = conn.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=CASE WHEN state='expired' AND GREATEST(media_expires_at+interval '1 hour',grant_cleanup_after)>clock_timestamp() THEN GREATEST(media_expires_at+interval '1 hour',grant_cleanup_after) ELSE clock_timestamp()+interval '24 hours' END WHERE id=$1`, id)
		swept = err == nil
		return err
	})
	return swept, err
}

// Keep cleanup's synchronous work bounded independently of the long media job.
func (s *RecordingPackageStore) ReconcilePackages(ctx context.Context, deleteObjects func(context.Context, []string) error) error {
	started := time.Now()
	ids, cutoff, err := s.packageCleanupCandidates(ctx)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var swept, skipped, failed int
	var failures error
	for _, id := range ids {
		if err := cleanupCtx.Err(); err != nil {
			failures = errors.Join(failures, err)
			break
		}
		completed, err := s.cleanupPackage(cleanupCtx, id, deleteObjects)
		switch {
		case err != nil:
			failed++
			failures = errors.Join(failures, err)
		case completed:
			swept++
		default:
			skipped++
		}
	}
	// IDs come from the package primary key without a multiplying join.
	attrs := []any{"cutoff", cutoff, "batch", 1, "limit", packageCleanupLimit,
		"selected", len(ids), "unique", len(ids), "limit_reached", len(ids) == packageCleanupLimit,
		"swept", swept, "skipped", skipped, "failed", failed, "unattempted", len(ids) - swept - skipped - failed}
	attrs = append(attrs, s.measurePackageCleanupRemaining(cleanupCtx, cutoff)...)
	slog.InfoContext(ctx, "recording_package_cleanup_batch", append(attrs, "elapsed_ms", time.Since(started).Milliseconds())...)
	return failures
}

func (s *RecordingPackageStore) measurePackageCleanupRemaining(ctx context.Context, cutoff time.Time) []any {
	// This best-effort read shares the cleanup deadline and cannot fail cleanup.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return []any{"remaining_measured", false}
	}
	eligible := fmt.Sprintf(packageCleanupEligibleAt, "$1::timestamptz")
	var due, overdue int64
	var oldest sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT count(*),
 count(*) FILTER (WHERE cleanup_after<$1::timestamptz-interval '1 hour'), min(cleanup_after)
 FROM recording_packages p WHERE cleanup_after<=$1::timestamptz AND `+eligible, cutoff).Scan(&due, &overdue, &oldest)
	if err != nil {
		return []any{"remaining_measured", false}
	}
	var oldestSeconds int64
	if oldest.Valid {
		oldestSeconds = int64(cutoff.Sub(oldest.Time) / time.Second)
	}
	return []any{"remaining_measured", true, "remaining_due", due, "remaining_overdue", overdue, "oldest_due_seconds", oldestSeconds}
}
