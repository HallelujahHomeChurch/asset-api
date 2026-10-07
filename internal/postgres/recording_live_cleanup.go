package postgres

import (
	"context"
	"fmt"
	"hhc/asset-api/internal/assets"
)

// The six-hour writer grace covers the existing worker's 5.5-hour deadline.
// Failed remote captures retain seven days; normal VOD final objects are owned
// by the package retention policy and never enumerated by this cleanup.
const captureCleanupSafe = `GREATEST(c.expires_at+interval '6 hours',
 CASE WHEN c.state IN ('failed','expired','aborted') OR c.package_id IS NULL THEN COALESCE(c.terminal_at,c.expires_at)+interval '7 days' ELSE c.expires_at END,
 l.read_grant_until,l.claimed_until)<=clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM recording_processing_slots s WHERE s.job_id IN (c.id,'live:'||c.id) AND s.leased_until>clock_timestamp())`

func (s *RecordingCaptureStore) ReconcileLive(ctx context.Context, deleteObjects func(context.Context, []string) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE l.cleanup_after<=clock_timestamp() AND `+captureCleanupSafe+` ORDER BY l.cleanup_after LIMIT 10`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
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
	for _, id := range ids {
		if err = s.cleanupLive(ctx, id, deleteObjects); err != nil {
			return err
		}
	}
	return nil
}
func (s *RecordingCaptureStore) cleanupLive(ctx context.Context, id string, deleteObjects func(context.Context, []string) error) error {
	// No new writer or grant can pass capture expiry. Rechecking the deadline and
	// lease fences immediately before remote work avoids reviving an active scope.
	var revision int64
	var safe bool
	err := s.db.QueryRowContext(ctx, `SELECT l.revision,`+captureCleanupSafe+` FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.id=$1`, id).Scan(&revision, &safe)
	if err != nil || !safe {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE recording_live SET cleanup_after=clock_timestamp()+interval '5 minutes' WHERE capture_id=$1`, id); err != nil {
		return err
	}
	c, err := scanCapture(s.db.QueryRowContext(ctx, `SELECT `+captureColumns+` FROM recording_captures WHERE id=$1`, id))
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM recording_capture_objects WHERE capture_id=$1 ORDER BY path`, id)
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		paths = append(paths, path)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	remove := func(keys []string) error {
		for len(keys) > 0 {
			n := min(1000, len(keys))
			if err := deleteObjects(ctx, keys[:n]); err != nil {
				return err
			}
			keys = keys[n:]
		}
		return nil
	}
	staging, live := []string{}, []string{}
	prefix := "recordings/captures/" + id + "/final/"
	for _, path := range paths {
		staging = append(staging, (assets.RecordingPackage{ID: id}).StagingKey(path))
		if path != "master.m3u8" && path != "1080p/index.m3u8" && path != "720p/index.m3u8" && path != "480p/index.m3u8" {
			live = append(live, prefix+path)
		}
	}
	live = append(live, prefix+"current.json")
	for rev := int64(1); rev <= revision; rev++ {
		for _, name := range []string{"master.m3u8", "1080p/index.m3u8", "720p/index.m3u8", "480p/index.m3u8"} {
			live = append(live, fmt.Sprintf("%splaylists/%d/%s", prefix, rev, name))
		}
	}
	if err = remove(staging); err != nil {
		return err
	}
	if err = remove(live); err != nil {
		return err
	}
	// Repeated sweeps are intentional: late provider completion can follow a timeout.
	attempts, err := s.db.QueryContext(ctx, `SELECT claim_id,sequence FROM recording_live_attempts WHERE capture_id=$1 ORDER BY purged,created_at LIMIT 100`, id)
	if err != nil {
		return err
	}
	type attempt struct {
		id  string
		seq int
	}
	var work []attempt
	for attempts.Next() {
		var a attempt
		if err = attempts.Scan(&a.id, &a.seq); err != nil {
			attempts.Close()
			return err
		}
		work = append(work, a)
	}
	err = attempts.Err()
	attempts.Close()
	if err != nil {
		return err
	}
	for _, a := range work {
		keys := []string{}
		for _, r := range assets.LiveRenditions() {
			base := "recordings/packages/" + c.ID + "/final/" + a.id + "/" + r.Name + "/"
			keys = append(keys, base+"init.mp4", fmt.Sprintf("%sseg-%06d.m4s", base, a.seq))
		}
		if err = remove(keys); err != nil {
			return err
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE recording_live_attempts SET purged=true WHERE claim_id=$1`, a.id); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE recording_live SET cleanup_after=CASE WHEN EXISTS(SELECT 1 FROM recording_live_attempts WHERE capture_id=$1 AND NOT purged) THEN clock_timestamp()+interval '1 minute' ELSE clock_timestamp()+interval '24 hours' END WHERE capture_id=$1`, id)
	return err
}
