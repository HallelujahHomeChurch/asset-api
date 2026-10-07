package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"time"
)

type RecordingLiveSnapshot struct {
	Segments          []assets.RecordingLiveSegment
	Revision          int64
	PublishedRevision int64
	Ended             bool
}
type RecordingLiveClaim struct {
	EndOnly  bool
	Capture  assets.RecordingCapture
	ClaimID  string
	Sequence int
	Objects  []assets.RecordingPackageObject
	Snapshot RecordingLiveSnapshot
}

func readLiveSnapshot(ctx context.Context, tx *sql.Tx, id string) (RecordingLiveSnapshot, error) {
	var value RecordingLiveSnapshot
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT segments,revision,published_revision,ended FROM recording_live WHERE capture_id=$1 FOR UPDATE`, id).Scan(&raw, &value.Revision, &value.PublishedRevision, &value.Ended)
	if errors.Is(err, sql.ErrNoRows) {
		return value, assets.ErrNotFound
	}
	if err != nil {
		return value, err
	}
	err = json.Unmarshal(raw, &value.Segments)
	return value, err
}

// Capture transactions always take owner -> capture -> live -> slot. Package
// workers use slot -> package and never lock a capture row in the opposite order.
func (s *RecordingCaptureStore) liveTransaction(ctx context.Context, id, claim string, fn func(*sql.Tx, assets.RecordingCapture, RecordingLiveSnapshot) error) error {
	tx, err := beginRecordingPolicyTx(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recording string
	if err = tx.QueryRowContext(ctx, `SELECT recording_id FROM recording_captures WHERE id=$1`, id).Scan(&recording); errors.Is(err, sql.ErrNoRows) {
		return assets.ErrNotFound
	} else if err != nil {
		return err
	}
	if err = checkRecordingNotDeleted(ctx, tx, recording); err != nil {
		return err
	}
	c, err := scanCapture(tx.QueryRowContext(ctx, `SELECT `+captureColumns+` FROM recording_captures WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if c.TerminalAt != nil || !time.Now().Before(c.ExpiresAt) || (c.State != "uploading" && c.State != "freezing" && c.State != "validating" && c.State != "ready") {
		return assets.ErrConflict
	}
	if c.PackageID != nil {
		var state string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM recording_packages WHERE id=$1`, *c.PackageID).Scan(&state); err != nil {
			return err
		}
		if state == "failed" || state == "expired" {
			return assets.ErrConflict
		}
	}
	snapshot, err := readLiveSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if claim != "" {
		var slot int
		err = tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE job_id=$1 AND claim_id=$2 AND leased_until>clock_timestamp() AND EXISTS(SELECT 1 FROM recording_live WHERE capture_id=$3 AND claim_id=$2 AND claimed_until>clock_timestamp()) FOR UPDATE`, "live:"+id, claim, id).Scan(&slot)
		if errors.Is(err, sql.ErrNoRows) {
			return assets.ErrConflict
		}
		if err != nil {
			return err
		}
	}
	if err = fn(tx, c, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *RecordingCaptureStore) ClaimLiveValidation(ctx context.Context) (RecordingLiveClaim, error) {
	var claim RecordingLiveClaim
	if err := s.reconcileTerminalPackages(ctx); err != nil {
		return claim, err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT c.id FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.state IN ('uploading','freezing','validating','ready') AND c.terminal_at IS NULL AND NOT (`+capturePackageTerminal+`) AND c.expires_at>clock_timestamp() AND l.next_attempt_at<=clock_timestamp() AND (l.claimed_until IS NULL OR l.claimed_until<=clock_timestamp()) AND (l.revision>l.published_revision OR (NOT l.ended AND c.package_id IS NOT NULL AND jsonb_array_length(c.inventory->'renditions')=3 AND jsonb_array_length(l.segments)>0 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(c.inventory->'renditions') r WHERE (r->>'segmentCount')::int<>jsonb_array_length(l.segments))) OR (NOT l.ended AND jsonb_array_length(l.segments)<1440 AND (SELECT count(*) FROM recording_capture_objects o WHERE o.capture_id=c.id AND o.state IN ('queued','verified') AND (o.path IN ('1080p/init.mp4','720p/init.mp4','480p/init.mp4') OR o.path IN ('1080p/seg-'||lpad(jsonb_array_length(l.segments)::text,6,'0')||'.m4s','720p/seg-'||lpad(jsonb_array_length(l.segments)::text,6,'0')||'.m4s','480p/seg-'||lpad(jsonb_array_length(l.segments)::text,6,'0')||'.m4s')))=6)) ORDER BY c.created_at LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, assets.ErrNotFound
	}
	if err != nil {
		return claim, err
	}
	err = s.liveTransaction(ctx, id, "", func(tx *sql.Tx, c assets.RecordingCapture, snapshot RecordingLiveSnapshot) error {
		var available bool
		if err := tx.QueryRowContext(ctx, `SELECT (claimed_until IS NULL OR claimed_until<=clock_timestamp()) AND next_attempt_at<=clock_timestamp() FROM recording_live WHERE capture_id=$1`, id).Scan(&available); err != nil {
			return err
		}
		if !available {
			return assets.ErrNotFound
		}
		var slot int
		if err := tx.QueryRowContext(ctx, `SELECT slot FROM recording_processing_slots WHERE leased_until IS NULL OR leased_until<=clock_timestamp() ORDER BY slot FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&slot); errors.Is(err, sql.ErrNoRows) {
			return assets.ErrNotFound
		} else if err != nil {
			return err
		}
		claim = RecordingLiveClaim{Capture: c, ClaimID: newStoreID(), Sequence: len(snapshot.Segments), Snapshot: snapshot}
		if c.PackageID != nil && c.Inventory != nil && len(c.Inventory.Renditions) == 3 && len(snapshot.Segments) > 0 && !snapshot.Ended {
			claim.EndOnly = true
			for _, r := range c.Inventory.Renditions {
				if r.SegmentCount != len(snapshot.Segments) {
					claim.EndOnly = false
				}
			}
		}
		if snapshot.Revision == snapshot.PublishedRevision && !claim.EndOnly {
			if snapshot.Ended || claim.Sequence >= 1440 {
				return assets.ErrNotFound
			}
			for _, r := range assets.LiveRenditions() {
				for _, path := range []string{r.Name + "/init.mp4", fmt.Sprintf("%s/seg-%06d.m4s", r.Name, claim.Sequence)} {
					var o assets.RecordingPackageObject
					o.Path = path
					if err := tx.QueryRowContext(ctx, `SELECT size_bytes,sha256 FROM recording_capture_objects WHERE capture_id=$1 AND path=$2 AND state IN ('queued','verified')`, id, path).Scan(&o.SizeBytes, &o.SHA256); errors.Is(err, sql.ErrNoRows) {
						return assets.ErrNotFound
					} else if err != nil {
						return err
					}
					claim.Objects = append(claim.Objects, o)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_live SET claim_id=$2,claimed_until=clock_timestamp()+interval '2 minutes' WHERE capture_id=$1`, id, claim.ClaimID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=$2,claim_id=$3,leased_until=clock_timestamp()+interval '2 minutes' WHERE slot=$1`, slot, "live:"+id, claim.ClaimID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO recording_live_attempts(claim_id,capture_id,sequence) VALUES($1,$2,$3)`, claim.ClaimID, id, min(claim.Sequence, 1439))
		return err
	})
	return claim, err
}
func (s *RecordingCaptureStore) HeartbeatLive(ctx context.Context, claim RecordingLiveClaim) error {
	return s.liveTransaction(ctx, claim.Capture.ID, claim.ClaimID, func(tx *sql.Tx, _ assets.RecordingCapture, _ RecordingLiveSnapshot) error {
		if _, err := tx.ExecContext(ctx, `UPDATE recording_live SET claimed_until=clock_timestamp()+interval '2 minutes' WHERE capture_id=$1`, claim.Capture.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET leased_until=clock_timestamp()+interval '2 minutes' WHERE claim_id=$1`, claim.ClaimID)
		return err
	})
}
func (s *RecordingCaptureStore) CommitLiveBatch(ctx context.Context, claim RecordingLiveClaim, batch map[string]assets.RecordingLiveFragment) (RecordingLiveSnapshot, error) {
	var result RecordingLiveSnapshot
	err := s.liveTransaction(ctx, claim.Capture.ID, claim.ClaimID, func(tx *sql.Tx, c assets.RecordingCapture, snapshot RecordingLiveSnapshot) error {
		if snapshot.Revision != snapshot.PublishedRevision || snapshot.Ended || len(snapshot.Segments) != claim.Sequence {
			return assets.ErrConflict
		}
		normalTail := c.PackageID != nil && c.Inventory != nil && len(c.Inventory.Renditions) == 3
		if normalTail {
			for _, r := range c.Inventory.Renditions {
				if r.SegmentCount != claim.Sequence+1 {
					normalTail = false
				}
			}
		}
		segments, err := assets.AppendLiveSegment(snapshot.Segments, batch, normalTail)
		if err != nil {
			return err
		}
		revision := int64(len(segments))
		if normalTail {
			revision++
		}
		raw, err := json.Marshal(segments)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recording_live SET segments=$2,revision=$3,ended=$4 WHERE capture_id=$1`, c.ID, raw, revision, normalTail); err != nil {
			return err
		}
		for _, o := range claim.Objects {
			result, err := tx.ExecContext(ctx, `UPDATE recording_capture_objects SET state='verified' WHERE capture_id=$1 AND path=$2 AND sha256=$3 AND size_bytes=$4 AND state IN ('queued','verified')`, c.ID, o.Path, o.SHA256, o.SizeBytes)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return assets.ErrConflict
			}
		}
		result = RecordingLiveSnapshot{Segments: segments, Revision: revision, PublishedRevision: snapshot.PublishedRevision, Ended: normalTail}
		return nil
	})
	return result, err
}
func (s *RecordingCaptureStore) FinishLivePublication(ctx context.Context, claim RecordingLiveClaim, revision int64) error {
	return s.liveTransaction(ctx, claim.Capture.ID, claim.ClaimID, func(tx *sql.Tx, c assets.RecordingCapture, snapshot RecordingLiveSnapshot) error {
		if snapshot.Revision != revision || len(snapshot.Segments) == 0 {
			return assets.ErrConflict
		}
		last := len(snapshot.Segments) - 1
		mediaEnd := min(float64(assets.RecordingMaxDurationSeconds), snapshot.Segments[last].Renditions["1080p"].End-snapshot.Segments[0].Renditions["1080p"].Start)
		if _, err := tx.ExecContext(ctx, `UPDATE recording_live SET published_revision=revision,published_sequence=$2,published_media_end=$3,last_advanced_at=clock_timestamp(),ended_at=CASE WHEN ended THEN COALESCE(ended_at,clock_timestamp()) ELSE NULL END,claim_id=NULL,claimed_until=NULL,attempts=0,next_attempt_at=clock_timestamp() WHERE capture_id=$1`, c.ID, last, mediaEnd); err != nil {
			return err
		}
		return releaseLiveClaim(ctx, tx, claim)
	})
}
func releaseLiveClaim(ctx context.Context, tx *sql.Tx, claim RecordingLiveClaim) error {
	if _, err := tx.ExecContext(ctx, `UPDATE recording_live_attempts SET finished_at=clock_timestamp() WHERE claim_id=$1`, claim.ClaimID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE recording_processing_slots SET job_id=NULL,claim_id=NULL,leased_until=NULL WHERE claim_id=$1`, claim.ClaimID)
	return err
}
func (s *RecordingCaptureStore) FailLiveClaim(ctx context.Context, claim RecordingLiveClaim, permanent bool) error {
	return s.liveTransaction(ctx, claim.Capture.ID, claim.ClaimID, func(tx *sql.Tx, c assets.RecordingCapture, _ RecordingLiveSnapshot) error {
		if _, err := tx.ExecContext(ctx, `UPDATE recording_live SET claim_id=NULL,claimed_until=NULL,attempts=attempts+1,next_attempt_at=clock_timestamp()+make_interval(secs=>LEAST(60,5*(attempts+1))) WHERE capture_id=$1`, c.ID); err != nil {
			return err
		}
		if permanent {
			if _, err := tx.ExecContext(ctx, `UPDATE recording_captures SET state='failed',terminal_at=clock_timestamp(),terminal_reason='live_invalid' WHERE id=$1`, c.ID); err != nil {
				return err
			}
		}
		return releaseLiveClaim(ctx, tx, claim)
	})
}
func (s *RecordingCaptureStore) LiveProgress(ctx context.Context, id string) (assets.RecordingLiveProgress, error) {
	value := assets.RecordingLiveProgress{LastSequence: -1}
	err := s.db.QueryRowContext(ctx, `SELECT published_revision,published_sequence,published_media_end,last_advanced_at,ended_at,ended AND published_revision=revision FROM recording_live WHERE capture_id=$1`, id).Scan(&value.Revision, &value.LastSequence, &value.MediaEndSeconds, &value.LastAdvancedAt, &value.EndedAt, &value.Ended)
	if errors.Is(err, sql.ErrNoRows) {
		return value, assets.ErrNotFound
	}
	return value, err
}

func (s *RecordingCaptureStore) CommitLiveEnd(ctx context.Context, claim RecordingLiveClaim) (RecordingLiveSnapshot, error) {
	var result RecordingLiveSnapshot
	err := s.liveTransaction(ctx, claim.Capture.ID, claim.ClaimID, func(tx *sql.Tx, c assets.RecordingCapture, snapshot RecordingLiveSnapshot) error {
		if snapshot.PublishedRevision != snapshot.Revision || snapshot.Ended || len(snapshot.Segments) == 0 || c.PackageID == nil || c.Inventory == nil || len(c.Inventory.Renditions) != 3 {
			return assets.ErrConflict
		}
		for _, r := range c.Inventory.Renditions {
			if r.SegmentCount != len(snapshot.Segments) {
				return assets.ErrConflict
			}
		}
		snapshot.Ended = true
		snapshot.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE recording_live SET revision=$2,ended=true WHERE capture_id=$1`, c.ID, snapshot.Revision); err != nil {
			return err
		}
		result = snapshot
		return nil
	})
	return result, err
}
