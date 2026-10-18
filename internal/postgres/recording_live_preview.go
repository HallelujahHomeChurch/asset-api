package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
)

type RecordingLivePreviewClaim struct {
	CaptureID, ClaimID, VTT string
	Completed, Last         int
	Timeline                []assets.RecordingLiveSegment
	Objects                 []assets.RecordingPackageObject
}

// Already claimed validation owns its slot; only waiting work preempts optional media.
const previewWorkWaiting = `NOT EXISTS(SELECT 1 FROM recording_packages WHERE state IN ('freezing','validating') AND validation_attempts<3 AND next_attempt_at<=now() AND (claimed_until IS NULL OR claimed_until<=now())) AND NOT EXISTS(SELECT 1 FROM recording_sources WHERE state IN ('finalizing','queued','processing') AND processing_attempts<3 AND retry_until>now() AND (claimed_until IS NULL OR claimed_until<=now())) AND NOT EXISTS(SELECT 1 FROM recording_live l JOIN recording_captures c ON c.id=l.capture_id WHERE c.state IN ('uploading','freezing','validating','ready') AND c.terminal_at IS NULL AND c.expires_at>now() AND l.next_attempt_at<=now() AND (l.claimed_until IS NULL OR l.claimed_until<=now()) AND (l.revision>l.published_revision OR (NOT l.ended AND c.package_id IS NOT NULL AND jsonb_array_length(c.inventory->'renditions')=3 AND jsonb_array_length(l.segments)>0 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(c.inventory->'renditions') r WHERE (r->>'segmentCount')::int<>jsonb_array_length(l.segments))) OR NOT l.ended AND (SELECT count(*) FROM recording_capture_objects o WHERE o.capture_id=c.id AND o.state IN ('queued','verified') AND (o.path IN ('480p/init.mp4','720p/init.mp4','1080p/init.mp4') OR o.path IN ('480p/seg-'||lpad(jsonb_array_length(l.segments)::text,6,'0')||'.m4s','720p/seg-'||lpad(jsonb_array_length(l.segments)::text,6,'0')||'.m4s','1080p/seg-'||lpad(jsonb_array_length(l.segments)::text,6,'0')||'.m4s')))=6))`

const livePreviewAlive = `cap.terminal_at IS NULL AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=cap.recording_id) AND (cap.state IN ('uploading','freezing','validating') AND cap.expires_at>now() OR cap.state='ready' AND EXISTS(SELECT 1 FROM recording_packages p WHERE p.id=cap.package_id AND p.state='ready' AND p.media_expires_at>now()))`

func (s *RecordingPackageStore) ClaimLivePreview(ctx context.Context) (RecordingLivePreviewClaim, error) {
	var c RecordingLivePreviewClaim
	if _, err := s.db.ExecContext(ctx, `INSERT INTO recording_live_previews(capture_id) SELECT l.capture_id FROM recording_live l JOIN recording_captures cap ON cap.id=l.capture_id WHERE l.published_sequence>=0 AND cap.state='uploading' AND cap.expires_at>now() AND NOT EXISTS(SELECT 1 FROM recording_live_previews p WHERE p.capture_id=l.capture_id) ORDER BY cap.created_at LIMIT 20 ON CONFLICT DO NOTHING`); err != nil {
		return c, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE leased_until IS NULL OR leased_until<=now() ORDER BY slot FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT p.capture_id,p.completed_sequence,p.vtt,LEAST(l.published_sequence,p.completed_sequence+6),l.segments FROM recording_live_previews p JOIN recording_live l ON l.capture_id=p.capture_id JOIN recording_captures cap ON cap.id=p.capture_id WHERE cap.state='uploading' AND p.completed_sequence<l.published_sequence AND p.next_attempt_at<=now() AND (p.claimed_until IS NULL OR p.claimed_until<=now()) AND `+livePreviewAlive+` AND `+previewWorkWaiting+` ORDER BY p.next_attempt_at FOR UPDATE OF p SKIP LOCKED LIMIT 1`).Scan(&c.CaptureID, &c.Completed, &c.VTT, &c.Last, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(raw, &c.Timeline); err != nil {
		return c, err
	}
	if c.Last < 0 || c.Last >= len(c.Timeline) {
		return c, assets.ErrInvalidUpload
	}
	c.Timeline = c.Timeline[:c.Last+1]
	rows, err := tx.QueryContext(ctx, `SELECT path,size_bytes,sha256 FROM recording_capture_objects WHERE capture_id=$1 AND state='verified' AND (path='480p/init.mp4' OR path LIKE '480p/seg-%.m4s')`, c.CaptureID)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var o assets.RecordingPackageObject
		if err = rows.Scan(&o.Path, &o.SizeBytes, &o.SHA256); err != nil {
			rows.Close()
			return c, err
		}
		c.Objects = append(c.Objects, o)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return c, err
	}
	c.ClaimID = newStoreID()
	if _, err = tx.ExecContext(ctx, `UPDATE recording_live_previews SET claim_id=$2,claimed_until=now()+interval '2 minutes' WHERE capture_id=$1`, c.CaptureID, c.ClaimID); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id='preview:'||$2,claim_id=$3,leased_until=now()+interval '2 minutes' WHERE slot=$1`, slot, c.CaptureID, c.ClaimID); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (s *RecordingPackageStore) FinishLivePreview(ctx context.Context, c RecordingLivePreviewClaim, vtt string, publish func(context.Context) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id='preview:'||$1 AND claim_id=$2 AND leased_until>now() FOR UPDATE`, c.CaptureID, c.ClaimID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT p.capture_id FROM recording_live_previews p JOIN recording_captures cap ON cap.id=p.capture_id WHERE p.capture_id=$1 AND p.claim_id=$2 AND p.claimed_until>now() AND `+livePreviewAlive+` FOR UPDATE OF p`, c.CaptureID, c.ClaimID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.ErrConflict
	}
	if err != nil {
		return err
	}
	ready := publish != nil
	if ready {
		if err = publish(ctx); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_live_previews SET completed_sequence=CASE WHEN $3 THEN $4 ELSE completed_sequence END,vtt=CASE WHEN $3 THEN $5 ELSE vtt END,claim_id=NULL,claimed_until=NULL,next_attempt_at=now()+CASE WHEN $3 THEN interval '0 seconds' ELSE interval '5 minutes' END WHERE capture_id=$1 AND claim_id=$2`, c.CaptureID, c.ClaimID, ready, c.Last, vtt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE slot=$1`, slot); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *RecordingPackageStore) LivePreviewProgress(ctx context.Context, id string) (int, string, error) {
	var last int
	var vtt string
	err := s.db.QueryRowContext(ctx, `SELECT completed_sequence,vtt FROM recording_live_previews WHERE capture_id=$1`, id).Scan(&last, &vtt)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, "WEBVTT\n\n", nil
	}
	return last, vtt, err
}
