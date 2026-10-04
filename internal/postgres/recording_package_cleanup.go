package postgres

import (
	"context"
	"fmt"
	"time"
)

const packageCleanupEligible = `(state IN ('ready','failed','expired') OR (state='uploading' AND expires_at<=clock_timestamp()) OR
 EXISTS (SELECT 1 FROM recording_package_attempts a WHERE a.package_id=p.id AND a.state IN ('failed','abandoned','purged')))`

func (s *RecordingPackageStore) PackageCleanupCandidates(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recording_packages p WHERE cleanup_after<=clock_timestamp() AND `+packageCleanupEligible+`
 ORDER BY cleanup_after LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Cleanup uses the same session lock as URL issuance and only deletes declared
// object paths. Failed deletion is retried; successful sweeps are repeated to
// remove late writes made with capabilities issued before complete/expiry.
func (s *RecordingPackageStore) CleanupPackage(ctx context.Context, id string, deleteObjects func(context.Context, []string) error) error {
	return s.WithSessionLock(ctx, id, func(ctx context.Context) error {
		p, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		var uploadExpired, mediaExpired, gracePassed, due bool
		err = s.locks.statements(ctx).QueryRowContext(ctx, `SELECT expires_at<=clock_timestamp(),COALESCE(media_expires_at<=clock_timestamp(),false),COALESCE(media_expires_at+interval '1 hour'<=clock_timestamp(),false),cleanup_after<=clock_timestamp() FROM recording_packages WHERE id=$1`, id).Scan(&uploadExpired, &mediaExpired, &gracePassed, &due)
		if err != nil {
			return err
		}
		if !due {
			return nil
		}
		if p.State == "uploading" && uploadExpired || p.State == "ready" && mediaExpired {
			if _, err := s.locks.statements(ctx).ExecContext(ctx, `UPDATE recording_packages SET state='expired' WHERE id=$1 AND state=$2`, id, p.State); err != nil {
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
		if _, err := s.locks.statements(ctx).ExecContext(ctx, `UPDATE recording_package_attempts a SET state='abandoned',finished_at=clock_timestamp() FROM recording_packages p WHERE a.package_id=p.id AND p.id=$1 AND a.state='processing' AND (a.claim_id IS DISTINCT FROM p.claim_id OR (p.state='failed' AND p.claimed_until<=clock_timestamp()))`, id); err != nil {
			return err
		}
		rows, err := s.locks.statements(ctx).QueryContext(ctx, `SELECT claim_id FROM recording_package_attempts WHERE package_id=$1 AND
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
			if _, err := s.locks.statements(ctx).ExecContext(ctx, `UPDATE recording_package_attempts SET state='purged' WHERE claim_id=$1 AND package_id=$2`, attempt, id); err != nil {
				return err
			}
		}
		// At most three preview attempts per package. Preserve all attempts
		// until media expiry + grant grace, including an ambiguous successful
		// pointer publication. Repeat declared-key sweeps to catch late writes.
		if gracePassed && p.FinalPrefix != "" {
			rows, err := s.locks.statements(ctx).QueryContext(ctx, `SELECT claim_id FROM recording_preview_attempts WHERE package_id=$1`, id)
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
		// ponytail: daily declared-key sweep retains metadata; add bounded metadata
		// compaction only after retention policy and provider late-write bounds agree.
		_, err = s.locks.statements(ctx).ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=clock_timestamp()+interval '24 hours' WHERE id=$1`, id)
		return err
	})
}

// Keep cleanup's synchronous work bounded independently of the long media job.
func (s *RecordingPackageStore) ReconcilePackages(ctx context.Context, deleteObjects func(context.Context, []string) error) error {
	ids, err := s.PackageCleanupCandidates(ctx)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, id := range ids {
		if err := s.CleanupPackage(cleanupCtx, id, deleteObjects); err != nil {
			return err
		}
	}
	return nil
}
